package pluginintegration

import "net/http"

const (
	differentialSyslogTCPDeliveryPolicy = "syslog-rfc5424-tcp-delivery"
	differentialSyslogGatewayPath       = "/logger/syslog"
)

// differentialSyslogCases derives one source-parity scenario from APISIX 3.17
// t/plugin/syslog.t TEST 4/5/7/8 and TEST 14/15/16 plus the pinned syslog and
// RFC5424 implementations. The http-tcp fixture keeps the origin and one raw
// RFC5424 sink frame on the single endpoint projected by the harness.
func differentialSyslogCases() []DifferentialCase {
	const routeID = "differential-syslog-tcp"

	return []DifferentialCase{{
		Name:             "syslog-sends-single-rfc5424-frame-over-tcp",
		Plugin:           "syslog",
		RouteID:          routeID,
		ComparisonPolicy: differentialSyslogTCPDeliveryPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": differentialSyslogGatewayPath,
				"plugins": map[string]any{
					"syslog": map[string]any{
						"host":      differentialFixtureHostPlaceholder,
						"port":      differentialFixturePortPlaceholder,
						"sock_type": "tcp", "tls": false,
						"log_format":  map[string]any{"case": "syslog"},
						"flush_limit": 1, "batch_max_size": 1,
						"inactive_timeout": 1, "max_retry_count": 0,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: differentialSyslogGatewayPath,
				Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-syslog-tcp", WireProtocol: "http-tcp",
			ExpectedCalls: 2, CollectTimeoutMillis: 6000,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
