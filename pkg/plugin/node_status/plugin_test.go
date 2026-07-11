package node_status

import (
	"net/http"
	"net/http/httptest"
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
			StatusHandler(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/orders", nil))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/apisix/status", nil))
	var response Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if response.Status["active"] != "1" || response.Status["accepted"] != "2" ||
		response.Status["handled"] != "1" || response.Status["total"] != "2" {
		t.Fatalf("status counters = %#v, want active=1 accepted=2 handled=1 total=2", response.Status)
	}
}
