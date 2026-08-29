package pluginintegration

import (
	"bytes"
	"reflect"
	"testing"
)

func TestDifferentialMQTTProxyCasePinsAPISIX317RejectedAndForwardedCONNECT(t *testing.T) {
	cases := differentialMQTTProxyCases()
	if len(cases) != 1 {
		t.Fatalf("mqtt-proxy cases = %d, want 1", len(cases))
	}
	mqttCase := cases[0]
	spec := mqttCase.Spec
	if spec.Name != "mqtt-proxy-rejects-invalid-then-forwards-connect" ||
		spec.Plugin != "mqtt-proxy" || spec.RouteID != "differential-mqtt-proxy-connect" {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.ComparisonPolicy != differentialMQTTProxyCONNECTPolicy {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	if got, want := mqttCase.SourceTests, []differentialMQTTSourceTest{
		{File: "t/stream-plugin/mqtt-proxy.t", TestNumbers: []int{2, 3}},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source tests = %#v, want %#v", got, want)
	}
	if len(spec.Steps) != 2 || spec.Steps[0].Request.Body != "mmm" {
		t.Fatalf("steps = %#v, want invalid header then CONNECT", spec.Steps)
	}
	wantConnect := []byte{
		0x10, 0x0f, 0x00, 0x04, 'M', 'Q', 'T', 'T',
		0x04, 0x02, 0x00, 0x3c, 0x00, 0x03, 'f', 'o', 'o',
	}
	if !bytes.Equal([]byte(spec.Steps[1].Request.Body), wantConnect) {
		t.Fatalf("CONNECT = %x, want pinned APISIX packet %x", []byte(spec.Steps[1].Request.Body), wantConnect)
	}
	if spec.Fixture.Name != "mqtt-broker" ||
		spec.Fixture.WireProtocol != differentialFixtureWireMQTTCONNECT ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	routes, ok := spec.Config["stream_routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("stream_routes = %#v", spec.Config["stream_routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["server_port"] != differentialMQTTListenPortPlaceholder {
		t.Fatalf("stream route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("stream route plugins = %#v", route["plugins"])
	}
	pluginConfig, ok := plugins["mqtt-proxy"].(map[string]any)
	if !ok || pluginConfig["protocol_name"] != "MQTT" || pluginConfig["protocol_level"] != 4 {
		t.Fatalf("mqtt-proxy config = %#v", plugins["mqtt-proxy"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok || upstream["type"] != "roundrobin" || upstream["scheme"] != "tcp" {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}

func TestDifferentialMQTTProxyRuntimeOverlayOwnsOnlyStreamListener(t *testing.T) {
	overlay := differentialMQTTProxyRuntimeOverlay("127.0.0.1:31985")
	apisix, ok := overlay["apisix"].(map[string]any)
	if !ok || apisix["proxy_mode"] != "http&stream" {
		t.Fatalf("apisix overlay = %#v", overlay["apisix"])
	}
	streamProxy, ok := apisix["stream_proxy"].(map[string]any)
	if !ok {
		t.Fatalf("stream_proxy = %#v", apisix["stream_proxy"])
	}
	if got, want := streamProxy["tcp"], []any{
		map[string]any{"addr": "127.0.0.1:31985"},
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("tcp listeners = %#v, want %#v", got, want)
	}
	if got, want := overlay["stream_plugins"], []any{"mqtt-proxy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stream_plugins = %#v, want %#v", got, want)
	}
}
