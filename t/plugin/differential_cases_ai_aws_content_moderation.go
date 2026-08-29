package pluginintegration

import "net/http"

// differentialAIAWSContentModerationCases maps APISIX 3.17
// t/plugin/ai-aws-content-moderation.t TEST 1/2 to the pinned test's local
// Comprehend endpoint and raw-body toxicity rejection. The case proves the
// HTTP/SigV4 protocol shape only; it does not claim a live AWS dependency.
func differentialAIAWSContentModerationCases() []DifferentialCase {
	const routeID = "differential-ai-aws-toxic-raw-body"

	return []DifferentialCase{{
		Name:             "ai-aws-content-moderation-toxic-raw-body",
		Plugin:           "ai-aws-content-moderation",
		RouteID:          routeID,
		ComparisonPolicy: "ai-aws-comprehend-sigv4",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/echo",
				"plugins": map[string]any{
					"ai-aws-content-moderation": map[string]any{
						"comprehend": map[string]any{
							"access_key_id":     "access",
							"secret_access_key": "ea+secret",
							"region":            "us-east-1",
							"endpoint":          "http://" + differentialFixturePlaceholder,
						},
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/echo",
			Host:   "gateway.example.test",
			Body:   "toxic",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"ResultList":[{"Toxicity":0.72150000333786,"Labels":[{"Name":"PROFANITY","Score":0.25589999556541}]}]}`,
			},
		},
		SecurityDecision: "deny",
	}}
}
