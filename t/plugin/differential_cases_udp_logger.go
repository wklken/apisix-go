package pluginintegration

import "net/http"

const (
	differentialUDPLoggerFixtureDeliveryPolicy = "udp-logger-fixture-delivery"
	differentialUDPLoggerFixtureMethod         = "UDP"
)

// differentialUDPLoggerCases derives one source-parity scenario from APISIX
// 3.17 t/plugin/udp-logger.t TEST 11/12 and apisix/utils/log-util.lua's matched
// route injection. It captures one origin request and one complete UDP datagram;
// @timestamp is the only dynamic payload field.
func differentialUDPLoggerCases() []DifferentialCase {
	const routeID = "differential-udp-logger-metadata-format"

	return []DifferentialCase{{
		Name:             "udp-logger-sends-single-metadata-format-datagram",
		Plugin:           "udp-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialUDPLoggerFixtureDeliveryPolicy,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id": "udp-logger",
				"log_format": map[string]any{
					"host":       "$host",
					"case name":  "logger format in plugin",
					"@timestamp": "$time_iso8601",
					"client_ip":  "$remote_addr",
				},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{
					"udp-logger": map[string]any{
						"host":           differentialFixtureHostPlaceholder,
						"port":           differentialFixturePortPlaceholder,
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/hello", Host: "localhost",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-udp-log", WireProtocol: "http-udp", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			Response:             DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world"},
		},
	}}
}
