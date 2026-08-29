package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAIRateLimitingCasesCoverPinnedAPISIX317CustomRejection(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAIRateLimitingCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAIRateLimitingCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "ai-rate-limiting-custom-rejection" || spec.Plugin != "ai-rate-limiting" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-ai-rate-custom-reject" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if spec.ComparisonPolicy != "ai-rate-limiting-window" {
		t.Fatalf("comparison policy = %q, want ai-rate-limiting-window", spec.ComparisonPolicy)
	}
	if len(spec.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(spec.Steps))
	}
	for index, step := range spec.Steps {
		if step.Request.Method != http.MethodPost || step.Request.Path != "/ai" ||
			step.Request.Host != "gateway.example.test" {
			t.Fatalf("step %d request = %#v", index, step.Request)
		}
		wantHeaders := map[string]string{
			"Authorization": "Bearer token",
			"Content-Type":  "application/json",
			"X-AI-Fixture":  "openai/chat-model-echo.json",
		}
		if !reflect.DeepEqual(step.Request.Headers, wantHeaders) {
			t.Fatalf("step %d headers = %#v, want %#v", index, step.Request.Headers, wantHeaders)
		}
		if step.Request.Body != `{ "messages": [ { "role": "system", "content": "You are a mathematician" }, { "role": "user", "content": "What is 1+1?"} ] }` {
			t.Fatalf("step %d body = %q", index, step.Request.Body)
		}
		wantDecision := "allow"
		if index == 3 {
			wantDecision = "deny"
		}
		if step.SecurityDecision != wantDecision {
			t.Fatalf("step %d security decision = %q, want %q", index, step.SecurityDecision, wantDecision)
		}
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 3 {
		t.Fatalf("fixture = %#v, want exactly three provider calls", spec.Fixture)
	}
	if spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "application/json" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}
	if spec.Fixture.Response.Body != `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"1 + 1 = 2.","role":"assistant"}}],"created":1723780938,"id":"chatcmpl-9wiSIg5LYrrpxwsr2PubSQnbtod1P","model":"gpt-35-turbo-instruct","object":"chat.completion","system_fingerprint":"fp_abc28019ad","usage":{"completion_tokens":5,"prompt_tokens":8,"total_tokens":10}}` {
		t.Fatalf("fixture response body = %q", spec.Fixture.Response.Body)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("routes = %#v, want prerequisite and tested route", spec.Config["routes"])
	}
	prerequisite, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("prerequisite route = %#v", routes[0])
	}
	prerequisitePlugins, ok := prerequisite["plugins"].(map[string]any)
	if !ok || len(prerequisitePlugins) != 1 {
		t.Fatalf("prerequisite plugins = %#v", prerequisite["plugins"])
	}
	if _, ok := prerequisitePlugins["prometheus"].(map[string]any); !ok {
		t.Fatalf("prometheus prerequisite = %#v", prerequisitePlugins["prometheus"])
	}
	route, ok := routes[1].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/ai" {
		t.Fatalf("tested route = %#v", routes[1])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	rateConfig, ok := plugins["ai-rate-limiting"].(map[string]any)
	if !ok || rateConfig["limit"] != 30 || rateConfig["time_window"] != 60 ||
		rateConfig["rejected_code"] != 403 || rateConfig["rejected_msg"] != "rate limit exceeded" {
		t.Fatalf("ai-rate-limiting config = %#v", plugins["ai-rate-limiting"])
	}
	proxyConfig, ok := plugins["ai-proxy"].(map[string]any)
	if !ok || proxyConfig["provider"] != "openai" || proxyConfig["ssl_verify"] != false {
		t.Fatalf("ai-proxy config = %#v", plugins["ai-proxy"])
	}
	override, ok := proxyConfig["override"].(map[string]any)
	if !ok || override["endpoint"] != "http://"+differentialFixturePlaceholder {
		t.Fatalf("ai-proxy override = %#v", proxyConfig["override"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}
