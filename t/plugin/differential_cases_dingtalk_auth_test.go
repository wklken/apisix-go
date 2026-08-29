package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialDingTalkAuthCasesCoverPinnedAPISIX317HeaderCodeFlow(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialDingTalkAuthCases()
	if len(cases) != 1 {
		t.Fatalf("differentialDingTalkAuthCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "dingtalk-auth-header-code-provider-flow" ||
		spec.Plugin != "dingtalk-auth" || spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialDingTalkAuthFixtureOAuthPolicy ||
		spec.SecurityDecision != "allow" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["X-DingTalk-Code"] != "valid_code" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 3 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" ||
		spec.Fixture.Response.Body != `{"accessToken":"access-token-a","errcode":0,"errmsg":"ok","result":{"userid":"user-a","name":"Alice"}}` {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["dingtalk-auth"].(map[string]any)
	if pluginConfig["access_token_url"] != "http://"+differentialFixturePlaceholder+"/v1.0/oauth2/accessToken" ||
		pluginConfig["userinfo_url"] != "http://"+differentialFixturePlaceholder+"/topapi/v2/user/getuserinfo" {
		t.Fatalf("dingtalk-auth endpoints = %#v", pluginConfig)
	}
}
