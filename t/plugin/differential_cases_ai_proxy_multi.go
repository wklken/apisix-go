package pluginintegration

import "net/http"

// differentialAIProxyMultiCases maps APISIX 3.17
// t/plugin/ai-proxy-multi.t TEST 5/7. The authenticated empty request is
// rejected before the selected OpenAI instance endpoint is called. The unused
// Prometheus route preserves the explicit source runtime prerequisite.
func differentialAIProxyMultiCases() []DifferentialCase {
	const routeID = "differential-ai-proxy-multi-empty-body"

	return []DifferentialCase{{
		Name:    "ai-proxy-multi-empty-request-body",
		Plugin:  "ai-proxy-multi",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-ai-proxy-multi-prometheus-dependency",
					"uri": "/_differential/ai-proxy-multi/prometheus",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/anything",
					"plugins": map[string]any{"ai-proxy-multi": map[string]any{
						"instances": []any{map[string]any{
							"name":     "openai-official",
							"provider": "openai",
							"weight":   1,
							"auth": map[string]any{"header": map[string]any{
								"Authorization": "Bearer token",
							}},
							"options": map[string]any{
								"model":       "gpt-4",
								"max_tokens":  512,
								"temperature": 1.0,
							},
							"override": map[string]any{
								"endpoint": "http://" + differentialFixturePlaceholder,
							},
						}},
						"ssl_verify": false,
					}},
				},
			},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/anything",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Authorization": "Bearer token",
			},
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected provider call",
			},
		},
		SecurityDecision: "deny",
	}}
}
