package pluginintegration

import "net/http"

const differentialRocketMQLoggerPublishPolicy = "rocketmq-logger-publish"

// differentialRocketMQLoggerCases maps APISIX 3.17 source commit 9ef2ecab,
// t/plugin/rocketmq-logger-log-format.t TEST 4/5. The shared HTTP/RocketMQ
// fixture observes the proxied request and the successful RocketMQ publish;
// route discovery and producer housekeeping are protocol setup, not plugin
// behavior calls.
func differentialRocketMQLoggerCases() []DifferentialCase {
	const routeID = "differential-rocketmq-logger-format"
	return []DifferentialCase{{
		Name:             "rocketmq-logger-publishes-route-format-entry",
		Plugin:           "rocketmq-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialRocketMQLoggerPublishPolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/hello",
			"plugins": map[string]any{"rocketmq-logger": map[string]any{
				"nameserver_list": []any{
					differentialFixtureHostPlaceholder + ":" + differentialFixturePortPlaceholder,
				},
				"topic": "test2", "key": "key1", "tag": "tag1",
				"log_format": map[string]any{"x_ip": "$remote_addr"},
				"timeout":    1, "batch_max_size": 1,
				"buffer_duration": 1, "inactive_timeout": 1, "max_retry_count": 0,
			}},
			"upstream": differentialUpstream(),
		}}},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello", Host: "localhost",
		},
		Fixture: DifferentialFixture{
			Name: "origin-and-rocketmq-message", WireProtocol: differentialFixtureWireHTTPRocketMQ,
			Response:      DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world\n"},
			ExpectedCalls: 2, CaptureAllCalls: true, CollectTimeoutMillis: 6000,
			SemanticHeaders: []string{
				differentialRocketMQTagHeader,
				differentialRocketMQQueueIDHeader,
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
