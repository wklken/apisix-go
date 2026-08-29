package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAzureFunctionsCasesCoverPinnedAPISIX317KeyedInvocation(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAzureFunctionsCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAzureFunctionsCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "azure-functions-keyed-local-invocation" ||
		spec.Plugin != "azure-functions" || spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialAzureFunctionsFixtureInvocationPolicy ||
		spec.SecurityDecision != "allow" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/azure" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["X-Extra-Header"] != "MUST" ||
		spec.Fixture.Response.Body != "faas invoked" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["azure-functions"].(map[string]any)
	if pluginConfig["function_uri"] != "http://"+differentialFixturePlaceholder+"/httptrigger" {
		t.Fatalf("function_uri = %#v", pluginConfig["function_uri"])
	}
	authorization := pluginConfig["authorization"].(map[string]any)
	if authorization["apikey"] != "test_key" {
		t.Fatalf("authorization = %#v", authorization)
	}
}
