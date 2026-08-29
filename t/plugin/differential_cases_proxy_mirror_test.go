package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialProxyMirrorCasesCoverPinnedAPISIX317NormalDelivery(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialProxyMirrorCases()
	if len(cases) != 1 {
		t.Fatalf("differentialProxyMirrorCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "proxy-mirror-normal-delivery" || spec.Plugin != "proxy-mirror" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-proxy-mirror-normal" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet || step.Request.Path != "/hello" ||
		step.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", step.Request)
	}
	if step.SecurityDecision != "not_applicable" {
		t.Fatalf("security decision = %q, want not_applicable", step.SecurityDecision)
	}
	if spec.ComparisonPolicy != "" {
		t.Fatalf("comparison policy = %q, want exact", spec.ComparisonPolicy)
	}
	if spec.Fixture.Name != "primary-and-mirror" || spec.Fixture.ExpectedCalls != 2 {
		t.Fatalf("fixture = %#v, want exactly primary plus mirror calls", spec.Fixture)
	}
	if spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world\n" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}

	route := onlyDifferentialProxyMirrorRoute(t, spec)
	if route["id"] != spec.RouteID || route["uri"] != "/hello" {
		t.Fatalf("route = %#v", route)
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	config, ok := plugins["proxy-mirror"].(map[string]any)
	if !ok || config["host"] != "http://"+differentialFixturePlaceholder || len(config) != 1 {
		t.Fatalf("proxy-mirror config = %#v", plugins["proxy-mirror"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok || upstream["type"] != "roundrobin" || upstream["pass_host"] != "rewrite" ||
		upstream["upstream_host"] != "differential.example.test" {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || len(nodes) != 1 || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}

func onlyDifferentialProxyMirrorRoute(t *testing.T, spec DifferentialCase) map[string]any {
	t.Helper()
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("route = %#v", routes[0])
	}
	return route
}
