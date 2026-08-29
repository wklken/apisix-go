package pluginintegration

import "net/http"

// differentialAIRAGCases maps APISIX 3.17 t/plugin/ai-rag.t TEST 6/8.
// Empty input is rejected before either provider or the primary upstream.
func differentialAIRAGCases() []DifferentialCase {
	const routeID = "differential-ai-rag-empty-body"

	return []DifferentialCase{{
		Name:    "ai-rag-empty-request-body",
		Plugin:  "ai-rag",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/echo",
				"plugins": map[string]any{
					"ai-rag": map[string]any{
						"embeddings_provider": map[string]any{
							"azure_openai": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder + "/embeddings",
								"api_key":  "key",
							},
						},
						"vector_search_provider": map[string]any{
							"azure_ai_search": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder + "/search",
								"api_key":  "wrongkey",
							},
						},
					},
				},
				"upstream": map[string]any{
					"type":      "roundrobin",
					"nodes":     map[string]any{differentialFixturePlaceholder: 1},
					"scheme":    "http",
					"pass_host": "node",
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/echo",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "deny",
	}}
}
