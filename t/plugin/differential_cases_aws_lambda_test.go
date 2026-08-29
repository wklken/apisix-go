package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAWSLambdaCasesCoverPinnedAPISIX317LocalInvocation(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAWSLambdaCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAWSLambdaCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "aws-lambda-local-function-response" || spec.Plugin != "aws-lambda" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/aws" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != "aws lambda invoked" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "allow" ||
		spec.ComparisonPolicy != "fixture-owned-function-endpoint" {
		t.Fatalf("decision/policy = %q/%q, want allow/fixture-owned-function-endpoint",
			spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["aws-lambda"].(map[string]any)
	if pluginConfig["function_uri"] !=
		"http://"+differentialFixturePlaceholder+"/httptrigger" {
		t.Fatalf("aws-lambda config = %#v", pluginConfig)
	}
	if _, exists := pluginConfig["authorization"]; exists {
		t.Fatalf("local invocation case unexpectedly claims AWS authorization evidence: %#v", pluginConfig)
	}
}
