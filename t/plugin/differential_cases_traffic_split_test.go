package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialTrafficSplitCasesMatchPinnedAPISIX317PassHostBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialTrafficSplitCases()
	if len(cases) != 1 {
		t.Fatalf("differentialTrafficSplitCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "traffic-split-matched-upstream-pass-host" || spec.Plugin != "traffic-split" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/uri?name=jack" ||
		spec.Request.Host != "127.0.0.1" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "traffic-split" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want not_applicable/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	routeUpstream := route["upstream"].(map[string]any)
	if routeUpstream["retries"] != 0 {
		t.Fatalf("route retries = %#v, want 0", routeUpstream["retries"])
	}
	if nodes := routeUpstream["nodes"].(map[string]any); len(nodes) != 1 || nodes["127.0.0.1:1"] != 1 {
		t.Fatalf("route upstream nodes = %#v, want unreachable fallback", nodes)
	}
	pluginConfig := route["plugins"].(map[string]any)["traffic-split"].(map[string]any)
	rule := pluginConfig["rules"].([]any)[0].(map[string]any)
	match := rule["match"].([]any)[0].(map[string]any)
	wantVars := []any{"arg_name", "==", "jack"}
	gotVars := match["vars"].([]any)[0].([]any)
	if strings.Join([]string{gotVars[0].(string), gotVars[1].(string), gotVars[2].(string)}, "|") !=
		strings.Join([]string{wantVars[0].(string), wantVars[1].(string), wantVars[2].(string)}, "|") {
		t.Fatalf("match vars = %#v, want %#v", gotVars, wantVars)
	}
	weighted := rule["weighted_upstreams"].([]any)[0].(map[string]any)
	upstream := weighted["upstream"].(map[string]any)
	if upstream["type"] != "roundrobin" || upstream["pass_host"] != "pass" {
		t.Fatalf("traffic-split upstream = %#v", upstream)
	}
	if nodes := upstream["nodes"].(map[string]any); len(nodes) != 1 || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("traffic-split nodes = %#v, want fixture-only upstream", nodes)
	}
}
