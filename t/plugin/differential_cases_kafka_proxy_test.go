package pluginintegration

import "testing"

func TestDifferentialKafkaProxyCaseMapsAPISIX317PubSubListOffsetAndFetch(t *testing.T) {
	cases := differentialKafkaProxyCases()
	if len(cases) != 1 {
		t.Fatalf("kafka-proxy cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "kafka-proxy-lists-offset-and-fetches-record" ||
		spec.Plugin != "kafka-proxy" || spec.RouteID != "differential-kafka-proxy-pubsub" {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != "kafka-proxy-pubsub-record" ||
		spec.SecurityDecision != "not_applicable" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Request.Method != "KAFKA_PUBSUB" || spec.Request.Path != "/kafka" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "kafka-pubsub-record" ||
		spec.Fixture.WireProtocol != "http-kafka" || spec.Fixture.ExpectedCalls != 4 ||
		!spec.Fixture.CaptureAllCalls ||
		spec.Fixture.Response.Status != 200 || spec.Fixture.Response.Body != "unused" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	if route["id"] != spec.RouteID || route["uri"] != "/kafka" || route["enable_websocket"] != true {
		t.Fatalf("route identity = %#v", route)
	}
	plugins := route["plugins"].(map[string]any)
	if config, ok := plugins["kafka-proxy"].(map[string]any); !ok || len(config) != 0 {
		t.Fatalf("kafka-proxy config = %#v", plugins["kafka-proxy"])
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["type"] != "none" || upstream["scheme"] != "kafka" ||
		upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("Kafka upstream = %#v", upstream)
	}
}
