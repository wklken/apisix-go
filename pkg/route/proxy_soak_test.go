package route

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/store"
)

// TestProxyRuntimeSoak runs an opt-in bounded-concurrency soak against the
// public route handler and the same loopback topology as the benchmark
// corpus. It is skipped unless APISIX_GO_RUN_SOAK=1; APISIX_GO_SOAK_DURATION
// overrides the 30-minute default.
func TestProxyRuntimeSoak(t *testing.T) {
	if os.Getenv("APISIX_GO_RUN_SOAK") != "1" {
		t.Skip("set APISIX_GO_RUN_SOAK=1 to run the 30-minute proxy soak")
	}
	duration := 30 * time.Minute
	if raw := os.Getenv("APISIX_GO_SOAK_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("APISIX_GO_SOAK_DURATION: %v", err)
		}
		duration = parsed
	}

	const (
		workers     = 256
		routes      = 100
		nodes       = 10
		payloadSize = 1024
	)

	payload := bytes.Repeat([]byte("x"), payloadSize)
	upstreams := make([]*httptest.Server, 0, nodes)
	nodeSpecs := make([]string, 0, nodes)
	for range nodes {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(payloadSize))
			_, _ = w.Write(payload)
		}))
		upstreams = append(upstreams, server)
		parsed, err := url.Parse(server.URL)
		if err != nil {
			t.Fatalf("parse upstream URL: %v", err)
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			t.Fatalf("parse upstream port: %v", err)
		}
		nodeSpecs = append(nodeSpecs, fmt.Sprintf(
			`{"host":%q,"port":%d,"weight":1}`,
			parsed.Hostname(),
			port,
		))
	}
	defer func() {
		for _, upstream := range upstreams {
			upstream.Close()
		}
	}()
	nodesJSON := "[" + strings.Join(nodeSpecs, ",") + "]"

	soakEvents := make(chan *store.Event, 64)
	soakStore, err := store.Open(t.TempDir()+"/soak.db", soakEvents)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	previous := store.ReplaceGlobalStoreForTest(soakStore)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previous) })
	soakStore.Start()
	t.Cleanup(func() { _ = soakStore.Stop() })

	for index := range routes {
		id := fmt.Sprintf("soak-%d", index)
		uri := "/soak/target"
		if index > 0 {
			uri = fmt.Sprintf("/soak/filler/%06d", index)
		}
		route := fmt.Sprintf(
			`{"id":%q,"uri":%q,"methods":["GET"],"upstream":{"scheme":"http","nodes":%s}}`,
			id, uri, nodesJSON,
		)
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/routes/" + id)
		event.Value = []byte(route)
		soakEvents <- event
	}
	if err := soakStore.Sync(); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}

	builder := NewBuilder(soakStore)
	mux, err := builder.BuildStrict()
	if err != nil {
		t.Fatalf("BuildStrict() error = %v", err)
	}
	t.Cleanup(builder.Stop)

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        workers,
			MaxIdleConnsPerHost: workers,
			MaxConnsPerHost:     workers,
		},
	}

	var requests atomic.Int64
	var errors atomic.Int64
	stop := make(chan struct{})
	var workersWG sync.WaitGroup
	for range workers {
		workersWG.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				requests.Add(1)
				response, err := client.Get(server.URL + "/soak/target")
				if err != nil {
					errors.Add(1)
					continue
				}
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					errors.Add(1)
				}
				if err := response.Body.Close(); err != nil {
					errors.Add(1)
				}
				if response.StatusCode != http.StatusOK {
					errors.Add(1)
				}
			}
		})
	}

	warmup := 5 * time.Minute
	if duration < 10*time.Minute {
		warmup = duration / 2
	}
	time.Sleep(warmup)
	warmupGoroutines := runtime.NumGoroutine()
	var warmupMem runtime.MemStats
	runtime.ReadMemStats(&warmupMem)
	t.Logf(
		"soak warmed after %s: requests=%d errors=%d goroutines=%d heap-in-use=%d",
		warmup, requests.Load(), errors.Load(), warmupGoroutines, warmupMem.HeapInuse,
	)

	time.Sleep(duration - warmup)

	close(stop)
	workersWG.Wait()

	runtime.GC()
	runtime.GC()
	endGoroutines := runtime.NumGoroutine()
	var endMem runtime.MemStats
	runtime.ReadMemStats(&endMem)
	t.Logf(
		"soak finished: requests=%d errors=%d start-goroutines=%d end-goroutines=%d warm-heap=%d end-heap=%d",
		requests.Load(), errors.Load(), warmupGoroutines, endGoroutines, warmupMem.HeapInuse, endMem.HeapInuse,
	)

	if errors.Load() != 0 {
		t.Fatalf("soak errors = %d, want 0", errors.Load())
	}
	if endGoroutines > warmupGoroutines+32 {
		t.Fatalf(
			"final goroutines = %d, warmup baseline = %d (limit %d)",
			endGoroutines, warmupGoroutines, warmupGoroutines+32,
		)
	}
	if endMem.HeapInuse > warmupMem.HeapInuse*125/100 {
		t.Fatalf(
			"final heap in use = %d, warmed = %d (limit +25%%)",
			endMem.HeapInuse, warmupMem.HeapInuse,
		)
	}
}
