package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type recordingSplitHealthReporter struct {
	target string
	status int
}

func (r *recordingSplitHealthReporter) ReportHTTP(target string, status int) {
	r.target = target
	r.status = status
}

func (r *recordingSplitHealthReporter) ReportTCPFailure(string, bool) {
}

func TestApplyTrafficSplitOverrideRetainsHealthReporter(t *testing.T) {
	reporter := &recordingSplitHealthReporter{}
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme:         "http",
		Host:           "127.0.0.1:8080",
		HealthReporter: reporter,
		HealthTarget:   "http://127.0.0.1:8080",
	})

	applyTrafficSplitOverride(req)
	pxy.ReportHTTPOutcome(req, http.StatusServiceUnavailable)

	if reporter.target != "http://127.0.0.1:8080" {
		t.Fatalf("reported target = %q, want selected traffic-split target", reporter.target)
	}
	if reporter.status != http.StatusServiceUnavailable {
		t.Fatalf("reported status = %d, want 503", reporter.status)
	}
}

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
		Checks: map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{
					"http_statuses": []any{http.StatusInternalServerError},
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
