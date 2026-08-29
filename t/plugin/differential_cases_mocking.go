package pluginintegration

import "net/http"

// differentialMockingCases maps APISIX 3.17 t/plugin/mocking.t TEST 1/2 at
// compatibilityOracleSourceCommit. The APISIX/Go version header is disabled so
// the fixed example body and status remain an exact deterministic comparison.
func differentialMockingCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "mocking-returns-fixed-example",
			Plugin:  "mocking",
			RouteID: "diff-mocking-fixed-example",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-mocking-fixed-example",
					"uri": "/hello",
					"plugins": map[string]any{
						"mocking": map[string]any{
							"content_type":     "text/plain",
							"response_status":  200,
							"response_example": "hello world",
							"with_mock_header": false,
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
