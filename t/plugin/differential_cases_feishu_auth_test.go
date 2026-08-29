package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialFeishuAuthCasesCoverPinnedAPISIX317HeaderCodeFlow(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialFeishuAuthCases()
	if len(cases) != 1 {
		t.Fatalf("differentialFeishuAuthCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "feishu-auth-header-code-provider-flow" || spec.Plugin != "feishu-auth" ||
		spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialFeishuAuthFixtureOAuthPolicy ||
		spec.SecurityDecision != "allow" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["X-Feishu-Code"] != "passed" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 3 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != `{"access_token":"access-token-a","expires_in":7200,"code":0,"msg":"success","data":{"open_id":"ou-a","name":"Alice"}}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["feishu-auth"].(map[string]any)
	if pluginConfig["access_token_url"] != "http://"+differentialFixturePlaceholder+"/token" ||
		pluginConfig["userinfo_url"] != "http://"+differentialFixturePlaceholder+"/userinfo" ||
		pluginConfig["auth_redirect_uri"] != "https://example.com/callback" {
		t.Fatalf("feishu-auth endpoints = %#v", pluginConfig)
	}
}
