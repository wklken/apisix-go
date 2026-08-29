package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAIAWSContentModerationCasesCoverPinnedAPISIX317RawBodyToxicity(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAIAWSContentModerationCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAIAWSContentModerationCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "ai-aws-content-moderation-toxic-raw-body" ||
		spec.Plugin != "ai-aws-content-moderation" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/echo" ||
		spec.Request.Body != "toxic" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != `{"ResultList":[{"Toxicity":0.72150000333786,"Labels":[{"Name":"PROFANITY","Score":0.25589999556541}]}]}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "deny" ||
		spec.ComparisonPolicy != "ai-aws-comprehend-sigv4" {
		t.Fatalf("decision/policy = %q/%q, want deny/ai-aws-comprehend-sigv4",
			spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["ai-aws-content-moderation"].(map[string]any)
	comprehend := pluginConfig["comprehend"].(map[string]any)
	if comprehend["access_key_id"] != "access" ||
		comprehend["secret_access_key"] != "ea+secret" ||
		comprehend["region"] != "us-east-1" ||
		comprehend["endpoint"] != "http://"+differentialFixturePlaceholder {
		t.Fatalf("comprehend config = %#v", comprehend)
	}
	if _, exists := pluginConfig["fail_mode"]; exists {
		t.Fatalf("plugin config includes Go-only fail_mode: %#v", pluginConfig)
	}
}
