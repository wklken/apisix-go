package node_status

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestTrackReportsServerWideRequestCounters(t *testing.T) {
	activeRequests.Store(0)
	acceptedRequests.Store(0)
	handledRequests.Store(0)
	totalRequests.Store(0)
	t.Cleanup(func() {
		activeRequests.Store(0)
		acceptedRequests.Store(0)
		handledRequests.Store(0)
		totalRequests.Store(0)
	})

	handler := Track(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apisix/status" {
			StatusHandler("node-test")(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apisix/status", nil))
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want APISIX 3.17 text/plain response", got)
	}
	if strings.HasSuffix(recorder.Body.String(), "\n") {
		t.Fatalf("body has trailing newline: %q", recorder.Body.String())
	}
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if response.ID != "node-test" {
		t.Fatalf("node id = %q, want node-test", response.ID)
	}
	if response.Status["active"] != "1" || response.Status["accepted"] != "2" ||
		response.Status["handled"] != "1" || response.Status["total"] != "2" {
		t.Fatalf("status counters = %#v, want active=1 accepted=2 handled=1 total=2", response.Status)
	}
	for _, unsupported := range []string{"reading", "writing", "waiting"} {
		if _, ok := response.Status[unsupported]; ok {
			t.Fatalf("status includes unowned %q counter: %#v", unsupported, response.Status)
		}
	}
}

func TestStringUintBoundaryValues(t *testing.T) {
	tests := []struct {
		value uint64
		want  string
	}{
		{value: 0, want: "0"},
		{value: 1, want: "1"},
		{value: 1234567890, want: "1234567890"},
		{value: math.MaxUint64, want: "18446744073709551615"},
	}
	for _, test := range tests {
		if got := stringUint(test.value); got != test.want {
			t.Fatalf("stringUint(%d) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestTrackConcurrentIncrementDecrement(t *testing.T) {
	activeRequests.Store(0)
	acceptedRequests.Store(0)
	handledRequests.Store(0)
	totalRequests.Store(0)
	t.Cleanup(func() {
		activeRequests.Store(0)
		acceptedRequests.Store(0)
		handledRequests.Store(0)
		totalRequests.Store(0)
	})

	handler := Track(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	const workers = 32
	const requestsPerWorker = 100
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range requestsPerWorker {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))
			}
		})
	}
	wg.Wait()

	const want = workers * requestsPerWorker
	if got := acceptedRequests.Load(); got != want {
		t.Fatalf("accepted = %d, want %d", got, want)
	}
	if got := handledRequests.Load(); got != want {
		t.Fatalf("handled = %d, want %d", got, want)
	}
	if got := totalRequests.Load(); got != want {
		t.Fatalf("total = %d, want %d", got, want)
	}
	if got := activeRequests.Load(); got != 0 {
		t.Fatalf("active = %d, want 0 after all handlers complete", got)
	}
}
