package pluginintegration

import "net/http"

// differentialAttachConsumerLabelCases maps the authenticated consumer case
// in t/plugin/attach-consumer-label.yaml and APISIX 3.17
// t/plugin/attach-consumer-label.t TEST 5/8/9 to one differential case.
func differentialAttachConsumerLabelCases() []DifferentialCase {
	const routeID = "differential-attach-consumer-label-authenticated"

	return []DifferentialCase{
		{
			Name:    "attach-consumer-label-authenticated-consumer-labels",
			Plugin:  "attach-consumer-label",
			RouteID: routeID,
			Config: map[string]any{
				"consumers": []any{map[string]any{
					"username": "jack",
					"labels": map[string]any{
						"department": "devops",
						"company":    "api7",
					},
					"plugins": map[string]any{
						"key-auth": map[string]any{"key": "key-a"},
					},
				}},
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/echo",
					"plugins": map[string]any{
						"key-auth": map[string]any{},
						"attach-consumer-label": map[string]any{
							"headers": map[string]any{
								"X-Consumer-Department": "$department",
								"X-Consumer-Company":    "$company",
								"X-Consumer-Role":       "$role",
							},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/echo",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"apikey":          "key-a",
					"X-Consumer-Role": "admin",
				},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "done",
				},
			},
			SecurityDecision: "allow",
		},
	}
}
