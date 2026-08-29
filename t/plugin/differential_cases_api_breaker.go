package pluginintegration

import "net/http"

// differentialAPIBreakerCases maps APISIX 3.17 t/plugin/api-breaker.t
// TEST 13-15 to a deterministic sequence. Three upstream 500 responses reach
// the configured threshold; the fourth request receives the explicit breaker
// response without calling upstream.
func differentialAPIBreakerCases() []DifferentialCase {
	request := DifferentialRequest{
		Method: http.MethodGet,
		Path:   "/api_breaker?code=500",
		Host:   "gateway.example.test",
	}
	steps := make([]DifferentialStep, 4)
	for index := range steps {
		steps[index] = DifferentialStep{
			Request:          request,
			SecurityDecision: "not_applicable",
		}
	}

	const routeID = "differential-api-breaker-custom-response"
	return []DifferentialCase{{
		Name:    "api-breaker-custom-response-after-threshold",
		Plugin:  "api-breaker",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/api_breaker",
				"plugins": map[string]any{
					"api-breaker": map[string]any{
						"break_response_code": 502,
						"break_response_body": `{"message":"breaker opened."}`,
						"break_response_headers": []any{
							map[string]any{"key": "Content-Type", "value": "application/json"},
							map[string]any{"key": "Content-Type", "value": "application/json+v1"},
						},
						"unhealthy": map[string]any{"failures": 3},
						"healthy":   map[string]any{"successes": 3},
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: steps,
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 3,
			Response: DifferentialFixtureResponse{
				Status: http.StatusInternalServerError,
				Body:   "upstream failure",
			},
		},
	}}
}
