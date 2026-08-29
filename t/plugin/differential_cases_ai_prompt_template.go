package pluginintegration

import "net/http"

// differentialAIPromptTemplateCases maps the named rewrite behavior in
// APISIX 3.17 t/plugin/ai-prompt-template.t TEST 1 and TEST 3 to an exact case.
func differentialAIPromptTemplateCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "ai-prompt-template-named-model-rewrite",
		Plugin:  "ai-prompt-template",
		RouteID: "diff-ai-prompt-template-named",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-ai-prompt-template-named", "uri": "/template",
				"plugins": map[string]any{"ai-prompt-template": map[string]any{
					"templates": []any{map[string]any{
						"name": "programming question",
						"template": map[string]any{
							"model": "some model",
						},
					}},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/template", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"template_name":"programming question"}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "templated"},
		},
		SecurityDecision: "not_applicable",
	}}
}
