package pluginintegration

import "net/http"

// differentialAuthzCasdoorCases maps APISIX 3.17
// t/plugin/authz-casdoor.t TEST 7. A callback without the session established
// by the authorization redirect is a deterministic 503 rejection path.
func differentialAuthzCasdoorCases() []DifferentialCase {
	const routeID = "differential-authz-casdoor-no-session"
	return []DifferentialCase{{
		Name:             "authz-casdoor-callback-without-session",
		Plugin:           "authz-casdoor",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonPlatformOwnedErrorRepresentation,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/anything/*",
				"plugins": map[string]any{"authz-casdoor": map[string]any{
					"callback_url":  "http://gateway.example.test/anything/callback",
					"endpoint_addr": "http://" + differentialFixturePlaceholder,
					"client_id":     "7ceb9b7fda4a9061ec1c",
					"client_secret": "3416238e1edf915eac08b8fe345b2b95cdba7e04",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/anything/callback?code=aaa&state=bbb",
			Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}
