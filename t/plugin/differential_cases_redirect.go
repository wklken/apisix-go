package pluginintegration

import "net/http"

// differentialRedirectCases maps APISIX 3.17 t/plugin/redirect.t TEST 3/4
// at compatibilityOracleSourceCommit to one deterministic fixed-URI redirect.
func differentialRedirectCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:             "redirect-fixed-uri-301",
			Plugin:           "redirect",
			RouteID:          "diff-redirect-fixed-uri",
			ComparisonPolicy: differentialComparisonPlatformOwnedRedirectRepresentation,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-redirect-fixed-uri",
					"uri": "/hello",
					"plugins": map[string]any{
						"redirect": map[string]any{
							"uri":      "/test/add",
							"ret_code": 301,
						},
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
					Body:   "unused",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}
}
