package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialLimitCountCasesCoverPinnedAPISIX317RouteLimit(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialLimitCountCases()
	if len(cases) != 1 {
		t.Fatalf("differentialLimitCountCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "limit-count-two-allows-then-rejects" || spec.Plugin != "limit-count" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-limit-count-two" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(spec.Steps))
	}
	for index, step := range spec.Steps {
		if step.Request.Method != http.MethodGet || step.Request.Path != "/hello" ||
			step.Request.Host != "gateway.example.test" {
			t.Fatalf("step %d request = %#v", index, step.Request)
		}
		wantDecision := "allow"
		if index >= 2 {
			wantDecision = "deny"
		}
		if step.SecurityDecision != wantDecision {
			t.Fatalf("step %d security decision = %q, want %q", index, step.SecurityDecision, wantDecision)
		}
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 2 {
		t.Fatalf("fixture = %#v, want exactly two upstream calls", spec.Fixture)
	}
	if spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}
	if spec.ComparisonPolicy != "limit-count-fixed-window-response" {
		t.Fatalf("comparison policy = %q, want narrow fixed-window policy", spec.ComparisonPolicy)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/hello" {
		t.Fatalf("route = %#v", routes[0])
	}
	methods, ok := route["methods"].([]any)
	if !ok || len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("route methods = %#v, want GET", route["methods"])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	config, ok := plugins["limit-count"].(map[string]any)
	if !ok || config["count"] != 2 || config["time_window"] != 60 ||
		config["rejected_code"] != 503 || config["key"] != "remote_addr" {
		t.Fatalf("limit-count config = %#v", plugins["limit-count"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}
