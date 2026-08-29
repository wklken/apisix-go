package pluginintegration

import "net/http"

// differentialTrafficLabelCases maps APISIX 3.17 t/plugin/traffic-label.t
// TEST 3/5 at compatibilityOracleSourceCommit. The fixture observation proves
// that the matching action replaces the client-provided upstream header.
func differentialTrafficLabelCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "traffic-label-overrides-request-header",
			Plugin:  "traffic-label",
			RouteID: "diff-traffic-label-header-override",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-traffic-label-header-override",
					"uri": "/echo",
					"plugins": map[string]any{
						"traffic-label": map[string]any{
							"rules": []any{map[string]any{
								"match": []any{[]any{"arg_foo", "==", "bar"}},
								"actions": []any{map[string]any{
									"set_headers": map[string]any{"X-Route-Observer": 100},
								}},
							}},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/echo?foo=bar",
				Host:    "gateway.example.test",
				Headers: map[string]string{"X-Route-Observer": "200"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "traffic-label",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}
}
