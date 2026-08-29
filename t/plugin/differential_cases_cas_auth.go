package pluginintegration

import "net/http"

// differentialCASAuthCases maps APISIX 3.17 t/plugin/cas-auth.t TEST 14/15.
// The callback is rejected with 401 before ticket validation when the signed
// CAS_REQUEST_URI initiation cookie is absent, so neither dependency is called.
func differentialCASAuthCases() []DifferentialCase {
	const routeID = "differential-cas-auth-callback-no-cookie"

	return []DifferentialCase{{
		Name:             "cas-auth-callback-without-initiation-cookie",
		Plugin:           "cas-auth",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonCASAuthCallbackNosniff,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "methods": []any{http.MethodGet, http.MethodPost},
				"host": "127.0.0.3", "uri": "/*",
				"plugins": map[string]any{"cas-auth": map[string]any{
					"idp_uri":          "http://" + differentialFixturePlaceholder + "/realms/test/protocol/cas",
					"cas_callback_uri": "/cas_callback",
					"logout_uri":       "/logout",
					"cookie": map[string]any{
						"secret": "0123456789abcdef0123456789abcdef",
						"secure": false,
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/cas_callback?ticket=ST-test",
			Host:   "127.0.0.3",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected network call",
			},
		},
		SecurityDecision: "deny",
	}}
}
