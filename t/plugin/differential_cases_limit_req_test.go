package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialLimitReqCasesCoverPinnedAPISIX317LocalBurstBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialLimitReqCases()
	if len(cases) != 2 {
		t.Fatalf("differentialLimitReqCases() = %d cases, want 2", len(cases))
	}

	assertDifferentialLimitReqCase(t, cases[0], differentialLimitReqExpectation{
		name:          "limit-req-rate-four-burst-two-allows-four",
		routeID:       "differential-limit-req-rate-four",
		rate:          4,
		burst:         2,
		decisions:     []string{"allow", "allow", "allow", "allow"},
		expectedCalls: 4,
	})
	assertDifferentialLimitReqCase(t, cases[1], differentialLimitReqExpectation{
		name:          "limit-req-low-rate-small-burst-rejects-followups",
		routeID:       "differential-limit-req-low-rate",
		rate:          0.1,
		burst:         0.1,
		decisions:     []string{"allow", "deny", "deny", "deny"},
		expectedCalls: 1,
		policy:        "limit-req-burst-response",
	})
}

type differentialLimitReqExpectation struct {
	name          string
	routeID       string
	rate          any
	burst         any
	decisions     []string
	expectedCalls int
	policy        string
}

func assertDifferentialLimitReqCase(
	t *testing.T,
	spec DifferentialCase,
	want differentialLimitReqExpectation,
) {
	t.Helper()
	if spec.Name != want.name || spec.Plugin != "limit-req" {
		t.Fatalf("case identity = %q/%q, want %q/limit-req", spec.Name, spec.Plugin, want.name)
	}
	if spec.RouteID != want.routeID {
		t.Fatalf("route ID = %q, want %q", spec.RouteID, want.routeID)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if spec.ComparisonPolicy != want.policy {
		t.Fatalf("comparison policy = %q, want %q", spec.ComparisonPolicy, want.policy)
	}
	if len(spec.Steps) != len(want.decisions) {
		t.Fatalf("steps = %d, want %d", len(spec.Steps), len(want.decisions))
	}
	for index, step := range spec.Steps {
		if step.Request.Method != http.MethodGet || step.Request.Path != "/hello" ||
			step.Request.Host != "gateway.example.test" {
			t.Fatalf("step %d request = %#v", index, step.Request)
		}
		if step.SecurityDecision != want.decisions[index] {
			t.Fatalf("step %d security decision = %q, want %q", index, step.SecurityDecision, want.decisions[index])
		}
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != want.expectedCalls {
		t.Fatalf("fixture = %#v, want %d upstream calls", spec.Fixture, want.expectedCalls)
	}
	if spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != want.routeID || route["uri"] != "/hello" {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	config, ok := plugins["limit-req"].(map[string]any)
	if !ok || config["rate"] != want.rate || config["burst"] != want.burst ||
		config["rejected_code"] != 503 || config["key"] != "remote_addr" || len(config) != 4 {
		t.Fatalf("limit-req config = %#v", plugins["limit-req"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || len(nodes) != 1 || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}
