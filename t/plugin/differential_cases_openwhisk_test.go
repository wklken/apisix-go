package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialOpenWhiskCasesCoverPinnedAPISIX317ActionMapping(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialOpenWhiskCases()
	if len(cases) != 1 {
		t.Fatalf("differentialOpenWhiskCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "openwhisk-json-action-response" || spec.Plugin != "openwhisk" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/hello" ||
		spec.Request.Headers["Content-Type"] != "application/json" ||
		spec.Request.Body != `{"name":"world"}` {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != `{"statusCode":200,"headers":{"Content-Type":"application/json"},"body":"{\"hello\":\"world\"}"}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "allow" ||
		spec.ComparisonPolicy != differentialComparisonFixtureOwnedUpstreamEndpoint {
		t.Fatalf("decision/policy = %q/%q, want allow/fixture authority", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["openwhisk"].(map[string]any)
	if pluginConfig["api_host"] != "http://"+differentialFixturePlaceholder ||
		pluginConfig["service_token"] != "test:test" || pluginConfig["namespace"] != "guest" ||
		pluginConfig["action"] != "test-params" || pluginConfig["result"] != true {
		t.Fatalf("openwhisk config = %#v", pluginConfig)
	}
}
