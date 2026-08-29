package pluginintegration

import "net/http"

// differentialWorkflowCases maps APISIX 3.17 workflow.t TEST 10/11. Both
// rules match, so the exact observable contract is workflow's ordered rule
// selection and short-circuiting, not any broader child-action semantics.
func differentialWorkflowCases() []DifferentialCase {
	const routeID = "differential-workflow-first-match"

	return []DifferentialCase{
		{
			Name:    "workflow-first-matching-rule-stops",
			Plugin:  "workflow",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/*",
					"plugins": map[string]any{
						"workflow": map[string]any{
							"rules": []any{
								map[string]any{
									"case":    []any{[]any{"arg_foo", "==", "bar"}},
									"actions": []any{[]any{"return", map[string]any{"code": 403}}},
								},
								map[string]any{
									"case":    []any{[]any{"uri", "==", "/hello"}},
									"actions": []any{[]any{"return", map[string]any{"code": 401}}},
								},
							},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello?foo=bar",
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
		},
	}
}
