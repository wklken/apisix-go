package pluginintegration

import (
	"net/http"
	"slices"
	"testing"
)

func TestDifferentialNodeStatusCasesCoverPinnedAPISIX317StatusJSON(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialNodeStatusCases()
	if len(cases) != 1 {
		t.Fatalf("differentialNodeStatusCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "node-status-reports-json-counters" || spec.Plugin != "node-status" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-node-status-public-api" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.ComparisonPolicy != "node-status-json-counters" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/apisix/status" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.SecurityDecision != "not_applicable" {
		t.Fatalf("security decision = %q", spec.SecurityDecision)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want zero upstream calls", spec.Fixture)
	}
	if got := differentialRequiredPluginNames(cases); !slices.Equal(got, []string{"node-status", "public-api"}) {
		t.Fatalf("required plugins = %v, want node-status and public-api", got)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one public-api route", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/apisix/status" {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	if _, ok := plugins["public-api"].(map[string]any); !ok {
		t.Fatalf("public-api config = %#v", plugins["public-api"])
	}
}
