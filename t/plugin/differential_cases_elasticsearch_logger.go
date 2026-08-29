package pluginintegration

import "net/http"

// differentialElasticsearchLoggerCases maps APISIX 3.17
// t/plugin/elasticsearch-logger.t TEST 15/16. Both implementations probe the
// fixture for its version, proxy the gateway request, then post one NDJSON bulk
// entry carrying the configured custom format.
func differentialElasticsearchLoggerCases() []DifferentialCase {
	const routeID = "differential-elasticsearch-logger-delivery"

	return []DifferentialCase{{
		Name:             "elasticsearch-logger-posts-single-custom-entry",
		Plugin:           "elasticsearch-logger",
		RouteID:          routeID,
		ComparisonPolicy: "elasticsearch-logger-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/elasticsearch",
				"plugins": map[string]any{
					"elasticsearch-logger": map[string]any{
						"endpoint_addr":  "http://" + differentialFixturePlaceholder,
						"field":          map[string]any{"index": "services"},
						"log_format":     map[string]any{"custom_case": "elasticsearch-logger"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/elasticsearch", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "version-origin-and-bulk", ExpectedCalls: 3,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    `{"version":{"number":"8.10.2"}}`,
			},
		},
	}}
}
