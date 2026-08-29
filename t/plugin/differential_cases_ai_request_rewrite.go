package pluginintegration

import "net/http"

// differentialAIRequestRewriteCases maps APISIX 3.17
// t/plugin/ai-request-rewrite.t TEST 8. The LLM override and route upstream
// intentionally share the local fixture: the first response supplies the
// rewritten content and the second captured request proves that content was
// replayed to the upstream.
func differentialAIRequestRewriteCases() []DifferentialCase {
	const routeID = "differential-ai-request-rewrite-override"

	return []DifferentialCase{{
		Name:    "ai-request-rewrite-override-replays-body",
		Plugin:  "ai-request-rewrite",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/anything",
				"plugins": map[string]any{
					"ai-request-rewrite": map[string]any{
						"prompt":   "some prompt",
						"provider": "openai",
						"auth": map[string]any{
							"header": map[string]any{"Authorization": "Bearer token"},
						},
						"override": map[string]any{
							"endpoint": "http://" + differentialFixturePlaceholder + "/random",
						},
						"ssl_verify": false,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/anything",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body: "some random content",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 2,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"choices":[{"message":{"content":"path override works"}}]}`,
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
