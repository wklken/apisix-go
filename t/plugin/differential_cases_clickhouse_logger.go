package pluginintegration

import "net/http"

// differentialClickHouseLoggerCases maps APISIX 3.17
// t/plugin/clickhouse-logger.t TEST 1/4/6 and the custom-format contract used
// by TEST 12 to one origin call plus one captured ClickHouse INSERT request.
func differentialClickHouseLoggerCases() []DifferentialCase {
	const routeID = "differential-clickhouse-logger-delivery"

	return []DifferentialCase{{
		Name:             "clickhouse-logger-posts-single-formatted-entry",
		Plugin:           "clickhouse-logger",
		RouteID:          routeID,
		ComparisonPolicy: "clickhouse-logger-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/clickhouse",
				"plugins": map[string]any{
					"clickhouse-logger": map[string]any{
						"endpoint_addr": "http://" + differentialFixturePlaceholder + "/clickhouse",
						"user":          "default", "password": "differential-password",
						"database": "default", "logtable": "logs",
						"log_format":     map[string]any{"case": "clickhouse-logger"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/clickhouse", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-clickhouse", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders: []string{
				"Content-Type", "X-ClickHouse-Database", "X-ClickHouse-Key", "X-ClickHouse-User",
			},
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
