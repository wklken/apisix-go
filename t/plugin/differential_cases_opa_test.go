package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialOPACasesCoverPinnedAPISIX317StringReasonDenial(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialOPACases()
	if len(cases) != 1 {
		t.Fatalf("differentialOPACases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "opa-denies-with-string-reason" || spec.Plugin != "opa" ||
		spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialOPAFixtureDecisionPolicy ||
		spec.SecurityDecision != "deny" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodGet ||
		spec.Request.Path != "/test?test=abcd&user=carla" ||
		spec.Request.Headers["test-header"] != "only-for-test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != `{"result":{"allow":false,"reason":"Give you a string reason"}}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["opa"].(map[string]any)
	if pluginConfig["host"] != "http://"+differentialFixturePlaceholder ||
		pluginConfig["policy"] != "example" {
		t.Fatalf("opa config = %#v", pluginConfig)
	}
}
