package pluginintegration

import "net/http"

const differentialKafkaProxyPubSubPolicy = "kafka-proxy-pubsub-record"

// differentialKafkaProxyCases maps APISIX 3.17 t/pubsub/kafka.t TEST 3's
// successful last-offset and offset-14 fetch path. t/plugin/kafka-proxy.t is
// schema/encryption-only; it is deliberately not cited as runtime evidence.
func differentialKafkaProxyCases() []DifferentialCase {
	const routeID = "differential-kafka-proxy-pubsub"
	return []DifferentialCase{{
		Name:             "kafka-proxy-lists-offset-and-fetches-record",
		Plugin:           "kafka-proxy",
		RouteID:          routeID,
		ComparisonPolicy: differentialKafkaProxyPubSubPolicy,
		Config: map[string]any{"routes": []any{map[string]any{
			"id": routeID, "uri": "/kafka", "enable_websocket": true,
			"plugins": map[string]any{"kafka-proxy": map[string]any{}},
			"upstream": map[string]any{
				"type": "none", "scheme": "kafka",
				"nodes": map[string]any{differentialFixturePlaceholder: 1},
			},
		}}},
		Request: DifferentialRequest{
			Method: "KAFKA_PUBSUB", Path: "/kafka", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "kafka-pubsub-record", WireProtocol: differentialFixtureWireHTTPKafka,
			ExpectedCalls: 4, CaptureAllCalls: true, CollectTimeoutMillis: 3000,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "not_applicable",
	}}
}
