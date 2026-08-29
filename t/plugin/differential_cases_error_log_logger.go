package pluginintegration

import "net/http"

const (
	differentialErrorLogLoggerClickHouseDeliveryPolicy = "error-log-logger-clickhouse-fixture-delivery"
	differentialErrorLogLoggerClickHousePath           = "/clickhouse"
)

// differentialErrorLogLoggerCases maps APISIX 3.17
// t/plugin/error-log-logger-clickhouse.t TEST 3/4 to one deterministic WARN:
// basic-auth emits the warning, its anonymous consumer keeps the request on the
// origin path, and the same fixture captures the independent ClickHouse POST.
func differentialErrorLogLoggerCases() []DifferentialCase {
	const routeID = "differential-error-log-logger-clickhouse"

	return []DifferentialCase{{
		Name:             "error-log-logger-clickhouse-delivers-basic-auth-warning",
		Plugin:           "error-log-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialErrorLogLoggerClickHouseDeliveryPolicy,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id": "error-log-logger",
				"clickhouse": map[string]any{
					"user": "default", "password": "differential-password",
					"database": "default", "logtable": "logs",
					"endpoint_addr": "http://" + differentialFixturePlaceholder +
						differentialErrorLogLoggerClickHousePath,
				},
				"level": "WARN", "batch_max_size": 2, "inactive_timeout": 1,
				"max_retry_count": 0,
			}},
			"consumers": []any{map[string]any{
				"username": "anonymous", "plugins": map[string]any{},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/warn",
				"plugins": map[string]any{
					"basic-auth": map[string]any{
						"anonymous_consumer": "anonymous", "hide_credentials": true,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/warn", Host: "gateway.example.test",
				Headers: map[string]string{"Authorization": "Bearer definitely-not-basic"},
			},
			SecurityDecision: "allow",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-error-log-clickhouse", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000, RequestWindowQuietMillis: 1500,
			SemanticHeaders: []string{
				"Content-Type", "X-ClickHouse-Database", "X-ClickHouse-Key", "X-ClickHouse-User",
				"X-Consumer-Username",
			},
			Response: DifferentialFixtureResponse{Status: http.StatusNoContent},
		},
	}}
}
