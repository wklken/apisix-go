package pluginintegration

import "net/http"

// differentialWolfRBACCases maps APISIX 3.17 t/plugin/wolf-rbac.t TEST 5/13.
// A request without an RBAC token is rejected before the configured upstream.
func differentialWolfRBACCases() []DifferentialCase {
	const routeID = "differential-wolf-rbac-missing-token"

	return []DifferentialCase{{
		Name:    "wolf-rbac-missing-token",
		Plugin:  "wolf-rbac",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":   routeID,
				"uris": []any{"/hello*", "/wolf/rbac/*"},
				"plugins": map[string]any{
					"wolf-rbac": map[string]any{},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/hello",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "deny",
	}}
}
