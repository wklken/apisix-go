package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialAIRequestRewriteCasesCoverPinnedAPISIX317OverrideRewrite(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAIRequestRewriteCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAIRequestRewriteCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "ai-request-rewrite-override-replays-body" ||
		spec.Plugin != "ai-request-rewrite" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/anything" ||
		spec.Request.Headers["Content-Type"] != "text/plain" ||
		spec.Request.Body != "some random content" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 2 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != `{"choices":[{"message":{"content":"path override works"}}]}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want not_applicable/exact",
			spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["ai-request-rewrite"].(map[string]any)
	if pluginConfig["prompt"] != "some prompt" || pluginConfig["provider"] != "openai" ||
		pluginConfig["ssl_verify"] != false {
		t.Fatalf("ai-request-rewrite config = %#v", pluginConfig)
	}
	auth := pluginConfig["auth"].(map[string]any)["header"].(map[string]any)
	if auth["Authorization"] != "Bearer token" {
		t.Fatalf("auth header = %#v", auth)
	}
	override := pluginConfig["override"].(map[string]any)
	if override["endpoint"] != "http://"+differentialFixturePlaceholder+"/random" {
		t.Fatalf("override = %#v", override)
	}
	upstream := route["upstream"].(map[string]any)
	if nodes := upstream["nodes"].(map[string]any); nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", nodes)
	}
}
