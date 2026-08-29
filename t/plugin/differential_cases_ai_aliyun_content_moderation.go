package pluginintegration

import "net/http"

// differentialAIAliyunContentModerationCases maps APISIX 3.17
// t/plugin/ai-aliyun-content-moderation.t TEST 1/2. With no selected AI
// instance, the plugin fails before either its moderation endpoint or an
// upstream can be called.
func differentialAIAliyunContentModerationCases() []DifferentialCase {
	const routeID = "differential-ai-aliyun-missing-instance"

	return []DifferentialCase{{
		Name:    "ai-aliyun-content-moderation-missing-ai-instance",
		Plugin:  "ai-aliyun-content-moderation",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/chat",
				"plugins": map[string]any{
					"ai-aliyun-content-moderation": map[string]any{
						"endpoint":          "http://" + differentialFixturePlaceholder,
						"region_id":         "cn-shanghai",
						"access_key_id":     "fake-key-id",
						"access_key_secret": "fake-key-secret",
						"risk_level_bar":    "high",
						"check_request":     true,
						"fail_mode":         "error",
					},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/chat",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"X-AI-Fixture": "aliyun/chat-with-harmful.json",
			},
			Body: `{"prompt": "What is 1+1?"}`,
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
