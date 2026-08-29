package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialServerlessPreFunctionCasesCoverPinnedAPISIX317Exit(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialServerlessPreFunctionCases()
	if len(cases) != 1 {
		t.Fatalf("differentialServerlessPreFunctionCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "serverless-pre-function-exits-201" || spec.Plugin != "serverless-pre-function" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-serverless-pre-exit" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want not_applicable/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["serverless-pre-function"].(map[string]any)
	wantFunctions := []any{
		"return function() ngx.log(ngx.ERR, 'serverless pre function'); ngx.exit(201); end",
	}
	if !reflect.DeepEqual(pluginConfig["functions"], wantFunctions) {
		t.Fatalf("serverless functions = %#v, want APISIX 3.17 serverless.t TEST 9/10", pluginConfig["functions"])
	}
}
