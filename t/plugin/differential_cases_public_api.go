package pluginintegration

import "net/http"

// differentialPublicAPICases maps APISIX 3.17 t/plugin/public-api.t TEST 2/3.
// The initializer materializes wolf-rbac's generation-local user-info handler;
// the public-api route must dispatch to it and reject the missing token before
// the configured fallback upstream can be called.
func differentialPublicAPICases() []DifferentialCase {
	const routeID = "differential-public-api-userinfo"
	return []DifferentialCase{{
		Name:    "public-api-wolf-rbac-userinfo-missing-token",
		Plugin:  "public-api",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id":  "differential-public-api-wolf-init",
					"uri": "/_wolf-init",
					"plugins": map[string]any{
						"wolf-rbac": map[string]any{},
					},
				},
				map[string]any{
					"id":  routeID,
					"uri": "/apisix/plugin/wolf-rbac/user_info",
					"plugins": map[string]any{
						"public-api": map[string]any{},
					},
					"upstream": differentialUpstream(),
				},
			},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/apisix/plugin/wolf-rbac/user_info",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected upstream",
			},
		},
		SecurityDecision: "deny",
	}}
}
