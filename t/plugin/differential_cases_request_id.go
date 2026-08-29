package pluginintegration

import "net/http"

// differentialRequestIDCases maps APISIX 3.17 t/plugin/request-id.t:
// TEST 13/14 preserves a client-provided ID in the response, while TEST 8/9
// omits the response header when include_in_response is false. A fixed client
// value keeps both contracts exact without normalizing away the semantic header.
func differentialRequestIDCases() []DifferentialCase {
	newCase := func(name, routeID string, includeInResponse bool) DifferentialCase {
		return DifferentialCase{
			Name:    name,
			Plugin:  "request-id",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/opentracing",
					"plugins": map[string]any{
						"request-id": map[string]any{
							"include_in_response": includeInResponse,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/opentracing",
				Host:    "gateway.example.test",
				Headers: map[string]string{"X-Request-Id": "client-provided-id"},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "ok",
				},
			},
			SecurityDecision: "not_applicable",
		}
	}

	return []DifferentialCase{
		newCase(
			"request-id-preserves-client-id-in-response",
			"differential-request-id-preserve-client",
			true,
		),
		newCase(
			"request-id-omits-client-id-from-response",
			"differential-request-id-omit-response",
			false,
		),
	}
}
