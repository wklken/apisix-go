package pluginintegration

import "net/http"

const differentialFeishuAuthFixtureOAuthPolicy = "feishu-auth-fixture-oauth"

// differentialFeishuAuthCases maps APISIX 3.17 t/plugin/feishu-auth.t TEST 6
// to local token, userinfo, and upstream endpoints. Supplying the code through
// X-Feishu-Code keeps apisix-go's query-callback OAuth-state protection intact
// while still exercising the provider exchange and authenticated forwarding.
func differentialFeishuAuthCases() []DifferentialCase {
	const routeID = "differential-feishu-auth-header-code"
	const fixtureBody = `{"access_token":"access-token-a","expires_in":7200,"code":0,"msg":"success","data":{"open_id":"ou-a","name":"Alice"}}`

	return []DifferentialCase{{
		Name:             "feishu-auth-header-code-provider-flow",
		Plugin:           "feishu-auth",
		RouteID:          routeID,
		ComparisonPolicy: differentialFeishuAuthFixtureOAuthPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello", "methods": []any{http.MethodGet},
				"plugins": map[string]any{
					"feishu-auth": map[string]any{
						"app_id":            "123",
						"app_secret":        "456",
						"secret":            "my-secret-xyz",
						"auth_redirect_uri": "https://example.com/callback",
						"access_token_url":  "http://" + differentialFixturePlaceholder + "/token",
						"userinfo_url":      "http://" + differentialFixturePlaceholder + "/userinfo",
						"redirect_uri":      "/login",
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/hello",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"X-Feishu-Code": "passed",
			},
		},
		Fixture: DifferentialFixture{
			Name:            "primary",
			ExpectedCalls:   3,
			CaptureAllCalls: true,
			SemanticHeaders: []string{"Content-Type", "X-Userinfo"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    fixtureBody,
			},
		},
		SecurityDecision: "allow",
	}}
}
