package pluginintegration

import "net/http"

// differentialAIPromptGuardCases maps APISIX 3.17
// t/plugin/ai-prompt-guard.t TEST 5 through TEST 7 to an exact deny case.
func differentialAIPromptGuardCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "ai-prompt-guard-anchored-deny",
		Plugin:  "ai-prompt-guard",
		RouteID: "diff-ai-prompt-guard-deny",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-guard-deny", "uri": "/v1/chat/completions",
				"plugins": map[string]any{"ai-prompt-guard": map[string]any{
					"match_all_roles": true,
					"deny_patterns":   []any{"^badword$"},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/v1/chat/completions", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"messages":[{"role":"system","content":"badword"}]}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "deny",
	}}
}
