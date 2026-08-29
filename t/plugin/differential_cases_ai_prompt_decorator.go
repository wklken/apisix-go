package pluginintegration

import "net/http"

// differentialAIPromptDecoratorCases maps APISIX 3.17
// t/plugin/ai-prompt-decorator.t TEST 12 and TEST 13 to an exact case.
func differentialAIPromptDecoratorCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "ai-prompt-decorator-append-responses-input",
		Plugin:  "ai-prompt-decorator",
		RouteID: "diff-ai-prompt-decorator-append",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-decorator-append", "uri": "/v1/responses",
				"plugins": map[string]any{"ai-prompt-decorator": map[string]any{
					"append": []any{map[string]any{
						"role": "user", "content": "Please be concise",
					}},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/v1/responses", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"input":"What is 1+1?"}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "decorated"},
		},
		SecurityDecision: "not_applicable",
	}}
}
