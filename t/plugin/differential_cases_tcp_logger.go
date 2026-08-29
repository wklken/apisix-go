package pluginintegration

import "net/http"

const (
	differentialTCPLoggerFixtureDeliveryPolicy = "tcp-logger-fixture-delivery"
	differentialTCPLoggerFixtureMethod         = "TCP"
)

// differentialTCPLoggerCases derives one source-parity scenario from APISIX
// 3.17 t/plugin/tcp-logger.t TEST 12/13 and apisix/utils/log-util.lua's custom
// format precedence and matched-resource injection. It captures one origin
// request and one complete, single-object TCP frame.
func differentialTCPLoggerCases() []DifferentialCase {
	const routeID = "differential-tcp-logger-route-format"

	return []DifferentialCase{{
		Name:             "tcp-logger-sends-single-route-format-frame",
		Plugin:           "tcp-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialTCPLoggerFixtureDeliveryPolicy,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id": "tcp-logger",
				"log_format": map[string]any{
					"case name": "metadata should lose",
				},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{
					"tcp-logger": map[string]any{
						"host": differentialFixtureHostPlaceholder,
						"port": differentialFixturePortPlaceholder,
						"tls":  false,
						"log_format": map[string]any{
							"case name":  "logger format in plugin",
							"service_id": "stale-service",
							"status":     "$status",
							"vip":        "$remote_addr",
						},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/hello", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-tcp-log", WireProtocol: "http-tcp", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			Response:             DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world"},
		},
	}}
}
