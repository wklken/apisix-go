package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuildReverseHandlerQuarantinesPassiveHTTPFailure(t *testing.T) {
	var badHits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	var goodHits atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		goodHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()

	upstream := resource.Upstream{
		Scheme: "http",
		Nodes: []resource.Node{
			upstreamNode(t, bad.URL),
			upstreamNode(t, good.URL),
		},
		Checks: map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{
					"http_statuses": []interface{}{http.StatusInternalServerError},
					"http_failures": 1,
				},
			},
		},
	}

	handler, err := (&Builder{}).buildReverseHandler(resource.Route{Upstream: upstream}, resource.Service{})
	if err != nil {
		t.Fatalf("buildReverseHandler() error = %v", err)
	}

	badResponses := 0
	for range 8 {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test/health", nil)
		handler.ServeHTTP(recorder, req)
		if recorder.Code == http.StatusInternalServerError {
			badResponses++
		}
	}
	if badResponses != 1 {
		t.Fatalf("passive health returned %d bad responses, want exactly one initial failure", badResponses)
	}
	if badHits.Load() != 1 {
		t.Fatalf("bad upstream hits = %d, want one", badHits.Load())
	}
	if goodHits.Load() != 7 {
		t.Fatalf("good upstream hits = %d, want seven", goodHits.Load())
	}
}

func upstreamNode(t *testing.T, rawURL string) resource.Node {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, rawURL, nil)
	return resource.Node{Host: request.URL.Hostname(), Port: portNumber(t, request.URL.Port()), Weight: 1}
}

func portNumber(t *testing.T, rawPort string) int {
	t.Helper()
	var port int
	if _, err := fmt.Sscanf(rawPort, "%d", &port); err != nil {
		t.Fatalf("parse test server port %q: %v", rawPort, err)
	}
	return port
}
