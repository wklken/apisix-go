package route

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/store"
)

// Benchmark corpus for the full proxy control path: route matching, plugin
// middleware, upstream selection, retry/timeout transport wrappers,
// ReverseProxy, and response copying, through a loopback upstream. The
// payload stays at 1 KiB so the row isolates control-path cost; payload
// scaling is covered by BenchmarkReverseProxyServeHTTP.

// The route store is a process-wide singleton, so sub-benchmarks share one
// store bound to a single events channel and reseed it with their own route
// set per row; Close() removes the published routes instead of stopping the
// store.

var (
	proxyLoopbackStoreOnce sync.Once
	proxyLoopbackStore     *store.Store
	proxyLoopbackEvents    chan *store.Event
	proxyLoopbackEnvMu     sync.Mutex
	proxyLoopbackEnvCache  = map[string]*proxyBenchmarkEnvironment{}
)

func proxyLoopbackRouteStore(b *testing.B) *store.Store {
	proxyLoopbackStoreOnce.Do(func() {
		proxyLoopbackEvents = make(chan *store.Event, 64)
		var err error
		proxyLoopbackStore, err = store.GetStore(
			b.TempDir()+"/proxy-loopback.db",
			proxyLoopbackEvents,
			testDataEncryptionService(),
		)
		if err != nil {
			b.Fatal(err)
		}
		proxyLoopbackStore.Start()
	})
	return proxyLoopbackStore
}

type proxyBenchmarkEnvironment struct {
	client     *http.Client
	server     *httptest.Server
	upstreams  []*httptest.Server
	storage    *store.Store
	builder    *Builder
	routeIDs   []string
	targetPath string
}

func (environment *proxyBenchmarkEnvironment) Close() {
	environment.server.Close()
	environment.builder.Stop()
	for _, upstream := range environment.upstreams {
		upstream.Close()
	}
	events := proxyLoopbackEvents
	for _, id := range environment.routeIDs {
		remove := store.NewEvent()
		remove.Type = store.EventTypeDelete
		remove.Key = []byte("/apisix/routes/" + id)
		events <- remove
	}
	_ = environment.storage.Sync()
}

func newProxyBenchmarkEnvironment(
	b *testing.B,
	routes, nodes, payloadSize int,
	plugins string,
) *proxyBenchmarkEnvironment {
	payload := bytes.Repeat([]byte("x"), payloadSize)
	environment := &proxyBenchmarkEnvironment{}
	pathPrefix := fmt.Sprintf("/bench/%d-%s-%d", routes, plugins, nodes)
	environment.targetPath = pathPrefix + "/target"

	nodeSpecs := make([]string, 0, nodes)
	for range nodes {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(payloadSize))
			_, _ = w.Write(payload)
		}))
		environment.upstreams = append(environment.upstreams, server)
		parsed, err := url.Parse(server.URL)
		if err != nil {
			b.Fatal(err)
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			b.Fatal(err)
		}
		nodeSpecs = append(nodeSpecs, fmt.Sprintf(
			`{"host":%q,"port":%d,"weight":1}`,
			parsed.Hostname(),
			port,
		))
	}
	nodesJSON := "[" + strings.Join(nodeSpecs, ",") + "]"

	storage := proxyLoopbackRouteStore(b)
	environment.storage = storage
	events := proxyLoopbackEvents

	pluginConfig := ""
	if plugins == "request-id" {
		pluginConfig = `{"request-id":{}}`
	}
	for index := range routes {
		id := fmt.Sprintf("bench-%d-%s-%d", routes, plugins, index)
		uri := environment.targetPath
		if index > 0 {
			uri = fmt.Sprintf("%s/filler/%06d", pathPrefix, index)
		}
		route := fmt.Sprintf(
			`{"id":%q,"uri":%q,"methods":["GET"],"upstream":{"scheme":"http","nodes":%s}}`,
			id, uri, nodesJSON,
		)
		if pluginConfig != "" {
			route = fmt.Sprintf(
				`{"id":%q,"uri":%q,"methods":["GET"],"plugins":%s,"upstream":{"scheme":"http","nodes":%s}}`,
				id, uri, pluginConfig, nodesJSON,
			)
		}
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/routes/" + id)
		event.Value = []byte(route)
		events <- event
		environment.routeIDs = append(environment.routeIDs, id)
	}
	if err := storage.Sync(); err != nil {
		b.Fatalf("Sync() error = %v", err)
	}

	builder := NewBuilder(storage, testEffectiveConfig(), testDataEncryptionResolver())
	mux, err := builder.BuildStrict()
	if err != nil {
		b.Fatal(err)
	}
	environment.builder = builder

	environment.server = httptest.NewServer(mux)

	concurrency := runtime.GOMAXPROCS(0) * 4
	environment.client = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        concurrency,
			MaxIdleConnsPerHost: concurrency,
			MaxConnsPerHost:     concurrency,
		},
	}
	return environment
}

func proxyLoopbackBenchmarkEnvironment(
	b *testing.B,
	routes, nodes, payloadSize int,
	plugins string,
) *proxyBenchmarkEnvironment {
	key := fmt.Sprintf("routes=%d/plugins=%s/nodes=%d/payload=%d", routes, plugins, nodes, payloadSize)
	proxyLoopbackEnvMu.Lock()
	defer proxyLoopbackEnvMu.Unlock()
	if environment := proxyLoopbackEnvCache[key]; environment != nil {
		return environment
	}
	environment := newProxyBenchmarkEnvironment(b, routes, nodes, payloadSize, plugins)
	proxyLoopbackEnvCache[key] = environment
	return environment
}

func BenchmarkRouteProxyLoopback(b *testing.B) {
	for _, routes := range []int{1, 100, 1000} {
		for _, plugins := range []string{"none", "request-id"} {
			for _, nodes := range []int{1, 10} {
				name := fmt.Sprintf("routes=%d/plugins=%s/nodes=%d", routes, plugins, nodes)
				b.Run(name, func(b *testing.B) {
					environment := proxyLoopbackBenchmarkEnvironment(b, routes, nodes, 1024, plugins)
					errors := make(chan error, 1)
					reportError := func(err error) {
						select {
						case errors <- err:
						default:
						}
					}
					b.ReportAllocs()
					b.SetBytes(1024)
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							response, err := environment.client.Get(environment.server.URL + environment.targetPath)
							if err != nil {
								reportError(err)
								return
							}
							if _, err := io.Copy(io.Discard, response.Body); err != nil {
								_ = response.Body.Close()
								reportError(err)
								return
							}
							if err := response.Body.Close(); err != nil {
								reportError(err)
								return
							}
							if response.StatusCode != http.StatusOK {
								reportError(fmt.Errorf("status = %d", response.StatusCode))
								return
							}
						}
					})
					b.StopTimer()
					select {
					case err := <-errors:
						b.Fatal(err)
					default:
					}
				})
			}
		}
	}
}
