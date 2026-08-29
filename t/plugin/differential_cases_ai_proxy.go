package pluginintegration

import "net/http"

// differentialAIProxyCases maps APISIX 3.17 t/plugin/ai-proxy.t TEST 5/7.
// The authenticated empty request is rejected before the configured OpenAI
// endpoint is called. The unused Prometheus route preserves the source
// runtime prerequisite for the exporter imported by ai-proxy's shared base.
func differentialAIProxyCases() []DifferentialCase {
	const routeID = "differential-ai-proxy-empty-body"

	return []DifferentialCase{{
		Name:    "ai-proxy-empty-request-body",
		Plugin:  "ai-proxy",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-ai-proxy-prometheus-dependency",
					"uri": "/_differential/ai-proxy/prometheus",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/anything",
					"plugins": map[string]any{"ai-proxy": map[string]any{
						"provider": "openai",
						"auth": map[string]any{"header": map[string]any{
							"Authorization": "Bearer token",
						}},
						"options": map[string]any{
							"model":       "gpt-35-turbo-instruct",
							"max_tokens":  512,
							"temperature": 1.0,
						},
						"override": map[string]any{
							"endpoint": "http://" + differentialFixturePlaceholder,
						},
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
