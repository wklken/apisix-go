package pluginintegration

import "net/http"

// differentialDataMaskCases maps APISIX 3.17 t/plugin/data-mask.t TEST 15-16.
// The HTTP logger is a local observer for the pinned access-log request_line:
// the origin must receive the untouched query while the detached log receives
// the password-free, replacement-masked request line.
func differentialDataMaskCases() []DifferentialCase {
	const routeID = "differential-data-mask-request-line"
	return []DifferentialCase{{
		Name:             "data-mask-sanitizes-logged-request-line",
		Plugin:           "data-mask",
		RouteID:          routeID,
		ComparisonPolicy: differentialDataMaskRequestLinePolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"data-mask": map[string]any{
						"request": []any{
							map[string]any{
								"type": "query", "name": "password", "action": "remove",
							},
							map[string]any{
								"type": "query", "name": "token", "action": "replace", "value": "*****",
							},
						},
					},
					"http-logger": map[string]any{
						"uri":             "http://" + differentialFixturePlaceholder + "/logs",
						"batch_max_size":  1,
						"max_retry_count": 0,
						"log_format": map[string]any{
							"request_line": "$request_line",
						},
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello?password=secret&token=mytoken",
				Host:   "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name:                 "origin-and-data-mask-log",
			ExpectedCalls:        2,
			CaptureAllCalls:      true,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "done",
			},
		},
	}}
}
