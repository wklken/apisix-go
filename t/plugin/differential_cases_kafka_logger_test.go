package pluginintegration

import (
	"reflect"
	"testing"
)

func TestDifferentialKafkaLoggerCasePinsAPISIX317OriginProduce(t *testing.T) {
	cases := differentialKafkaLoggerCases()
	if len(cases) != 1 {
		t.Fatalf("case count = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "kafka-logger-publishes-origin-request-body" || spec.Plugin != "kafka-logger" ||
		spec.ComparisonPolicy != differentialKafkaLoggerProducePolicy ||
		spec.Fixture.WireProtocol != differentialFixtureWireHTTPKafka ||
		spec.Fixture.ExpectedCalls != 2 || !spec.Fixture.CaptureAllCalls {
		t.Fatalf("case contract is not pinned: %#v", spec)
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	plugin := route["plugins"].(map[string]any)["kafka-logger"].(map[string]any)
	want := map[string]any{
		"broker_list": map[string]any{
			differentialFixtureHostPlaceholder: differentialFixturePortPlaceholder,
		},
		"kafka_topic": "test2", "key": "key1", "timeout": 1, "producer_type": "sync",
		"batch_max_size": 1, "include_req_body": true, "meta_format": "origin",
	}
	if !reflect.DeepEqual(plugin, want) {
		t.Fatalf("plugin config = %#v, want %#v", plugin, want)
	}
	if spec.Request.Path != "/hello?ab=cd" || spec.Request.Body != "abcdef" {
		t.Fatalf("request = %#v, want pinned query and body", spec.Request)
	}
}
