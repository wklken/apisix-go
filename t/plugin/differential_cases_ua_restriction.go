package pluginintegration

import "net/http"

// differentialUARestrictionCases maps APISIX 3.17
// t/plugin/ua-restriction.t TEST 6/7 at compatibilityOracleSourceCommit to a
// deterministic denylist rejection.
func differentialUARestrictionCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "ua-restriction-denies-listed-agent",
			Plugin:  "ua-restriction",
			RouteID: "diff-ua-restriction-deny",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-ua-restriction-deny",
					"uri": "/hello",
					"plugins": map[string]any{
						"ua-restriction": map[string]any{
							"denylist": []any{
								"my-bot1",
								`(Baiduspider)/(\d+)\.(\d+)`,
							},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/hello",
				Host:    "gateway.example.test",
				Headers: map[string]string{"User-Agent": "my-bot1"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 0,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "unused",
				},
			},
			SecurityDecision: "deny",
		},
	}
}
