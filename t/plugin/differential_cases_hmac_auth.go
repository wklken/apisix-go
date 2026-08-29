package pluginintegration

import "net/http"

// differentialHMACAuthCases maps APISIX 3.17 t/plugin/hmac-auth.t TEST 6/7.
// The missing Authorization rejection is observable without clocks or signing
// helpers, and an absent plugin would incorrectly reach the fixture upstream.
func differentialHMACAuthCases() []DifferentialCase {
	const routeID = "differential-hmac-auth-missing-authorization"
	return []DifferentialCase{{
		Name:    "hmac-auth-missing-authorization",
		Plugin:  "hmac-auth",
		RouteID: routeID,
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "jack",
				"plugins": map[string]any{"hmac-auth": map[string]any{
					"key_id": "my-access-key", "secret_key": "my-secret-key",
				}},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins":  map[string]any{"hmac-auth": map[string]any{}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}
