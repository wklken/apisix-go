package graphql_limit_count

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/resource"
)

func parityRequest(p *Plugin, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://example.com/graphql", strings.NewReader(query))
	r.Header.Set("Content-Type", "application/graphql")
	r.Header.Set("X-A", "same")
	r.Header.Set("X-B", "same")
	w := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
	return w
}

func TestParityGenerationRetainsQuota(t *testing.T) {
	cfg := Config{Count: 1, TimeWindow: 60}
	state := limitbase.NewState()
	t.Cleanup(state.Close)
	a := newTestPlugin(t, cfg)
	if setter, ok := any(a).(interface{ SetRateLimitState(*limitbase.State) }); ok {
		setter.SetRateLimitState(state)
	}
	a.SetResourceContext(resource.Route{ID: "same"}, resource.Service{})
	if w := parityRequest(a, "{viewer}"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	b := newTestPlugin(t, cfg)
	if setter, ok := any(b).(interface{ SetRateLimitState(*limitbase.State) }); ok {
		setter.SetRateLimitState(state)
	}
	b.SetResourceContext(resource.Route{ID: "same"}, resource.Service{})
	if w := parityRequest(b, "{viewer}"); w.Code != 503 {
		t.Fatalf("same config fresh generation status=%d, want503 retained quota", w.Code)
	}
}

func TestParityRulesShareResolvedKey(t *testing.T) {
	p := newTestPlugin(
		t,
		Config{
			Rules: []Rule{{Count: 1, TimeWindow: 60, Key: "$http_x_a"}, {Count: 1, TimeWindow: 60, Key: "$http_x_b"}},
		},
	)
	if w := parityRequest(p, "{viewer}"); w.Code != 503 {
		t.Fatalf("same resolved key across rules status=%d, want503 second rule consumes same key", w.Code)
	}
}

func TestParityRejectedBodyJSON(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 1, TimeWindow: 60, RejectedMsg: "quota"})
	parityRequest(p, "{viewer}")
	w := parityRequest(p, "{viewer}")
	if w.Header().Get("Content-Type") != "application/json" ||
		strings.TrimSpace(w.Body.String()) != `{"error_msg":"quota"}` {
		t.Fatalf("rejection content-type=%q body=%q", w.Header().Get("Content-Type"), w.Body.String())
	}
}

func TestParityDefaultCountExpression(t *testing.T) {
	p := newTestPlugin(t, Config{Count: "${http_x_count ?? 2}", TimeWindow: 60})
	if w := parityRequest(p, "{viewer}"); w.Code != 204 {
		t.Fatalf("default expression status=%d, want204; body=%s", w.Code, w.Body.String())
	}
}

func TestParityLocalRejectedCostConsumes(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 2, TimeWindow: 60})
	w := parityRequest(p, "{a{b{c}}}")
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if w := parityRequest(p, "{viewer}"); w.Code != 503 {
		t.Fatalf("post-rejected-cost status=%d, want503", w.Code)
	}
}
