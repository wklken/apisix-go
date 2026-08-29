package pluginintegration

import "net/http"

// differentialLimitCountCases maps APISIX 3.17 t/plugin/limit-count.t
// TEST 3/4. Four requests share one plugin instance: the configured quota
// allows the first two requests and rejects the next two before upstream.
func differentialLimitCountCases() []DifferentialCase {
	const routeID = "differential-limit-count-two"

	steps := make([]DifferentialStep, 0, 4)
	for index := range 4 {
		decision := "allow"
		if index >= 2 {
			decision = "deny"
		}
		steps = append(steps, DifferentialStep{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
			},
			SecurityDecision: decision,
		})
	}

	return []DifferentialCase{{
		Name:             "limit-count-two-allows-then-rejects",
		Plugin:           "limit-count",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonLimitCountFixedWindowResponse,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":      routeID,
				"uri":     "/hello",
				"methods": []any{http.MethodGet},
				"plugins": map[string]any{
					"limit-count": map[string]any{
						"count":         2,
						"time_window":   60,
						"rejected_code": 503,
						"key":           "remote_addr",
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: steps,
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 2,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world",
			},
		},
	}}
}
