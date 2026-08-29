package pluginintegration

import "net/http"

// differentialLimitReqCases maps APISIX 3.17 t/plugin/limit-req.t
// TEST 3-6. Each case runs four requests against one local limiter instance,
// preserving the quota state that the original pipelined requests share.
func differentialLimitReqCases() []DifferentialCase {
	return []DifferentialCase{
		newDifferentialLimitReqCase(
			"limit-req-rate-four-burst-two-allows-four",
			"differential-limit-req-rate-four",
			4,
			2,
			[]string{"allow", "allow", "allow", "allow"},
			4,
		),
		newDifferentialLimitReqCase(
			"limit-req-low-rate-small-burst-rejects-followups",
			"differential-limit-req-low-rate",
			0.1,
			0.1,
			[]string{"allow", "deny", "deny", "deny"},
			1,
		),
	}
}

func newDifferentialLimitReqCase(
	name string,
	routeID string,
	rate any,
	burst any,
	decisions []string,
	expectedCalls int,
) DifferentialCase {
	steps := make([]DifferentialStep, 0, len(decisions))
	for _, decision := range decisions {
		steps = append(steps, DifferentialStep{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
			},
			SecurityDecision: decision,
		})
	}

	comparisonPolicy := ""
	if name == "limit-req-low-rate-small-burst-rejects-followups" {
		comparisonPolicy = "limit-req-burst-response"
	}
	return DifferentialCase{
		Name:             name,
		Plugin:           "limit-req",
		RouteID:          routeID,
		ComparisonPolicy: comparisonPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"limit-req": map[string]any{
						"rate":          rate,
						"burst":         burst,
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
			ExpectedCalls: expectedCalls,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world",
			},
		},
	}
}
