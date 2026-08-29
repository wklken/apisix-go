package pluginintegration

import "net/http"

const differentialKafkaLoggerProducePolicy = "kafka-logger-produce"

// differentialKafkaLoggerCases maps APISIX 3.17 source commit 9ef2ecab,
// t/plugin/kafka-logger.t TEST 7/8. The shared HTTP/Kafka fixture observes the
// origin call and the successful Produce record without counting protocol
// negotiation as a plugin behavior call.
func differentialKafkaLoggerCases() []DifferentialCase {
	const routeID = "differential-kafka-logger-origin"
	return []DifferentialCase{{
		Name:             "kafka-logger-publishes-origin-request-body",
		Plugin:           "kafka-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialKafkaLoggerProducePolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id":  routeID,
			"uri": "/hello",
			"plugins": map[string]any{"kafka-logger": map[string]any{
				"broker_list": map[string]any{
					differentialFixtureHostPlaceholder: differentialFixturePortPlaceholder,
				},
				"kafka_topic": "test2", "key": "key1",
				"timeout": 1, "producer_type": "sync", "batch_max_size": 1,
				"include_req_body": true, "meta_format": "origin",
			}},
			"upstream": differentialUpstream(),
		}}},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello?ab=cd", Host: "localhost", Body: "abcdef",
		},
		Fixture: DifferentialFixture{
			Name: "origin-and-kafka-record", WireProtocol: differentialFixtureWireHTTPKafka,
			Response:      DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world\n"},
			ExpectedCalls: 2, CaptureAllCalls: true, CollectTimeoutMillis: 6000,
		},
		SecurityDecision: "not_applicable",
	}}
}
