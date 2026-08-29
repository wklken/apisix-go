package pluginintegration

import (
	"reflect"
	"testing"
)

func TestDifferentialRocketMQLoggerCasePinsAPISIX317RouteFormatPublish(t *testing.T) {
	cases := differentialRocketMQLoggerCases()
	if len(cases) != 1 {
		t.Fatalf("case count = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "rocketmq-logger-publishes-route-format-entry" ||
		spec.Plugin != "rocketmq-logger" ||
		spec.ComparisonPolicy != differentialRocketMQLoggerPublishPolicy ||
		spec.Fixture.WireProtocol != differentialFixtureWireHTTPRocketMQ ||
		spec.Fixture.ExpectedCalls != 2 || !spec.Fixture.CaptureAllCalls {
		t.Fatalf("case contract is not pinned: %#v", spec)
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one route", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	plugin := route["plugins"].(map[string]any)["rocketmq-logger"].(map[string]any)
	want := map[string]any{
		"nameserver_list": []any{
			differentialFixtureHostPlaceholder + ":" + differentialFixturePortPlaceholder,
		},
		"topic": "test2", "key": "key1", "tag": "tag1",
		"log_format": map[string]any{"x_ip": "$remote_addr"},
		"timeout":    1, "batch_max_size": 1,
		"buffer_duration": 1, "inactive_timeout": 1, "max_retry_count": 0,
	}
	if !reflect.DeepEqual(plugin, want) {
		t.Fatalf("plugin config = %#v, want %#v", plugin, want)
	}
	if spec.Request.Method != "GET" || spec.Request.Path != "/hello" || spec.Request.Host != "localhost" ||
		spec.Fixture.Response.Body != "hello world\n" {
		t.Fatalf("request/response contract = %#v / %#v", spec.Request, spec.Fixture.Response)
	}
	wantHeaders := []string{differentialRocketMQTagHeader, differentialRocketMQQueueIDHeader}
	if !reflect.DeepEqual(spec.Fixture.SemanticHeaders, wantHeaders) {
		t.Fatalf("semantic headers = %#v, want %#v", spec.Fixture.SemanticHeaders, wantHeaders)
	}
}
