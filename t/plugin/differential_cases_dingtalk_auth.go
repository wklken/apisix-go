package pluginintegration

import "net/http"

const differentialDingTalkAuthFixtureOAuthPolicy = "dingtalk-auth-fixture-oauth"

// differentialDingTalkAuthCases maps APISIX 3.17
// t/plugin/dingtalk-auth.t TEST 8 to local token, userinfo, and upstream
// endpoints. The authorization code is supplied through the configured header
// so both implementations exercise the provider flow without weakening the
// apisix-go OAuth-state protection for query callbacks.
func differentialDingTalkAuthCases() []DifferentialCase {
	const routeID = "differential-dingtalk-auth-header-code"
	const fixtureBody = `{"accessToken":"access-token-a","errcode":0,"errmsg":"ok","result":{"userid":"user-a","name":"Alice"}}`

	return []DifferentialCase{{
		Name:             "dingtalk-auth-header-code-provider-flow",
		Plugin:           "dingtalk-auth",
		RouteID:          routeID,
		ComparisonPolicy: differentialDingTalkAuthFixtureOAuthPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello", "methods": []any{http.MethodGet},
				"plugins": map[string]any{
					"dingtalk-auth": map[string]any{
						"app_key":          "testappkey",
						"app_secret":       "testappsecret",
						"secret":           "my-session-secret",
						"access_token_url": "http://" + differentialFixturePlaceholder + "/v1.0/oauth2/accessToken",
						"userinfo_url":     "http://" + differentialFixturePlaceholder + "/topapi/v2/user/getuserinfo",
						"redirect_uri":     "/login",
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
				"X-DingTalk-Code": "valid_code",
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
