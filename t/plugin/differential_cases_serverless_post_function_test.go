package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialServerlessPostFunctionCasesCoverPinnedAPISIX317EarlyStop(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialServerlessPostFunctionCases()
	if len(cases) != 1 {
		t.Fatalf("differentialServerlessPostFunctionCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "serverless-post-function-early-forbidden" || spec.Plugin != "serverless-post-function" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "deny" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want deny/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["serverless-post-function"].(map[string]any)
	wantFunctions := []any{
		"return function(conf, ctx) return 403, 'forbidden' end",
		"return function(conf, ctx) ngx.log(ngx.ERR, 'unreachable') end",
	}
	if !reflect.DeepEqual(pluginConfig["functions"], wantFunctions) {
		t.Fatalf("serverless functions = %#v, want exact TEST 25/26 pair", pluginConfig["functions"])
	}
}
