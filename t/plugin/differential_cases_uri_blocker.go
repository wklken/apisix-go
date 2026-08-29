package pluginintegration

import "net/http"

// differentialURIBlockerCases maps APISIX 3.17 t/plugin/uri-blocker.t
// TEST 17/18 at compatibilityOracleSourceCommit to one deterministic query
// rejection with the pinned custom response body.
func differentialURIBlockerCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "uri-blocker-rejects-matching-query",
			Plugin:  "uri-blocker",
			RouteID: "diff-uri-blocker-query-reject",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-uri-blocker-query-reject",
					"uri": "/hello",
					"plugins": map[string]any{
						"uri-blocker": map[string]any{
							"block_rules":  []any{"aa"},
							"rejected_msg": "access is not allowed",
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello?aa=1",
				Host:   "gateway.example.test",
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
