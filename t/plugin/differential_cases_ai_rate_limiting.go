package pluginintegration

import "net/http"

// differentialAIRateLimitingCases maps APISIX 3.17
// t/plugin/ai-rate-limiting.t TEST 4/5. Four requests share one plugin
// instance. The first three OpenAI responses each consume ten total tokens;
// the fourth request is rejected with the configured code and message before
// the provider fixture is called.
func differentialAIRateLimitingCases() []DifferentialCase {
	const routeID = "differential-ai-rate-custom-reject"
	const requestBody = `{ "messages": [ { "role": "system", "content": "You are a mathematician" }, { "role": "user", "content": "What is 1+1?"} ] }`

	steps := make([]DifferentialStep, 0, 4)
	for index := range 4 {
		decision := "allow"
		if index == 3 {
			decision = "deny"
		}
		steps = append(steps, DifferentialStep{
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/ai",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"Authorization": "Bearer token",
					"Content-Type":  "application/json",
					"X-AI-Fixture":  "openai/chat-model-echo.json",
				},
				Body: requestBody,
			},
			SecurityDecision: decision,
		})
	}

	return []DifferentialCase{{
		Name:             "ai-rate-limiting-custom-rejection",
		Plugin:           "ai-rate-limiting",
		RouteID:          routeID,
		ComparisonPolicy: "ai-rate-limiting-window",
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-ai-rate-prometheus-dependency",
					"uri": "/_differential/ai-rate/prometheus",
					"plugins": map[string]any{
						"prometheus": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/ai",
					"plugins": map[string]any{
						"ai-proxy": map[string]any{
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
						},
						"ai-rate-limiting": map[string]any{
							"limit":         30,
							"time_window":   60,
							"rejected_code": 403,
							"rejected_msg":  "rate limit exceeded",
						},
					},
					"upstream": differentialUpstream(),
				},
			},
		},
		Steps: steps,
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 3,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"1 + 1 = 2.","role":"assistant"}}],"created":1723780938,"id":"chatcmpl-9wiSIg5LYrrpxwsr2PubSQnbtod1P","model":"gpt-35-turbo-instruct","object":"chat.completion","system_fingerprint":"fp_abc28019ad","usage":{"completion_tokens":5,"prompt_tokens":8,"total_tokens":10}}`,
			},
		},
	}}
}
