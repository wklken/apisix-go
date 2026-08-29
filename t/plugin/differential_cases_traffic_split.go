package pluginintegration

import "net/http"

// differentialTrafficSplitCases maps APISIX 3.17
// t/plugin/traffic-split2.t TEST 4 and TEST 5 to one deterministic
// pass_host=pass request. The route upstream is deliberately unreachable so
// the case cannot pass unless traffic-split selects its matched upstream.
func differentialTrafficSplitCases() []DifferentialCase {
	const routeID = "differential-traffic-split-pass-host"

	return []DifferentialCase{{
		Name:    "traffic-split-matched-upstream-pass-host",
		Plugin:  "traffic-split",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/uri",
				"plugins": map[string]any{
					"traffic-split": map[string]any{
						"rules": []any{map[string]any{
							"match": []any{map[string]any{
								"vars": []any{[]any{"arg_name", "==", "jack"}},
							}},
							"weighted_upstreams": []any{map[string]any{
								"upstream": map[string]any{
									"type":      "roundrobin",
									"pass_host": "pass",
									"nodes":     map[string]any{differentialFixturePlaceholder: 1},
								},
							}},
						}},
					},
				},
				"upstream": map[string]any{
					"type":    "roundrobin",
					"retries": 0,
					"nodes":   map[string]any{"127.0.0.1:1": 1},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/uri?name=jack",
			Host:   "127.0.0.1",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "traffic-split",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
