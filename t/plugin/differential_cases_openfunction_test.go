package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialOpenFunctionCasesCoverPinnedAPISIX317BodyMapping(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialOpenFunctionCases()
	if len(cases) != 1 {
		t.Fatalf("differentialOpenFunctionCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "openfunction-post-body-response" || spec.Plugin != "openfunction" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/hello" ||
		spec.Request.Body != "test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != "Hello, test!" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "allow" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want allow/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["openfunction"].(map[string]any)
	if pluginConfig["function_uri"] != "http://"+differentialFixturePlaceholder+"/default/test-body" {
		t.Fatalf("openfunction config = %#v", pluginConfig)
	}
}
