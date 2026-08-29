package pluginintegration

import "net/http"

// differentialHTTPLoggerCases maps APISIX 3.17 t/plugin/http-logger.t
// TEST 1/2/4/5 to one proxied request and one captured single-entry log POST.
func differentialHTTPLoggerCases() []DifferentialCase {
	const routeID = "differential-http-logger-delivery"

	return []DifferentialCase{{
		Name:             "http-logger-posts-single-formatted-entry",
		Plugin:           "http-logger",
		RouteID:          routeID,
		ComparisonPolicy: "http-logger-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/http",
				"plugins": map[string]any{
					"http-logger": map[string]any{
						"uri":            "http://" + differentialFixturePlaceholder + "/http-log",
						"auth_header":    "Basic differential",
						"log_format":     map[string]any{"case": "http-logger"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/http", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-http-log", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response:             DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
