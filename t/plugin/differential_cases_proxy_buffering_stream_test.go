package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialProxyBufferingStreamingCasePinsIncrementalSSEBoundary(t *testing.T) {
	cases := differentialProxyBufferingStreamingCases()
	if len(cases) != 1 {
		t.Fatalf("streaming cases = %d, want 1", len(cases))
	}
	streamCase := cases[0]
	if streamCase.Spec.Name != "proxy-buffering-disabled-incremental-sse" ||
		streamCase.Spec.Plugin != "proxy-buffering" {
		t.Fatalf("case identity = %q/%q", streamCase.Spec.Name, streamCase.Spec.Plugin)
	}
	if streamCase.Spec.ComparisonPolicy != differentialProxyBufferingSSEPolicy {
		t.Fatalf("comparison policy = %q", streamCase.Spec.ComparisonPolicy)
	}
	if streamCase.Spec.Request.Method != http.MethodGet ||
		streamCase.Spec.Request.Path != "/events" ||
		streamCase.Spec.Request.Headers["Accept"] != "text/event-stream" {
		t.Fatalf("request = %#v", streamCase.Spec.Request)
	}
	if got, want := streamCase.SourceFiles, []string{
		"t/cli/test_proxy_buffering.sh",
		"t/cli/test_sse.py",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("source files = %#v, want %#v", got, want)
	}
	if streamCase.Contract.RequiredFrames != 3 || len(streamCase.Contract.Frames) != 3 {
		t.Fatalf("stream contract = %#v", streamCase.Contract)
	}
	if streamCase.Spec.Fixture.WireProtocol != differentialFixtureWireSSEHTTP ||
		streamCase.Spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("stream fixture dispatch = %#v", streamCase.Spec.Fixture)
	}

	routes, ok := streamCase.Spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", streamCase.Spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	config, ok := plugins["proxy-buffering"].(map[string]any)
	if !ok || config["disable_proxy_buffering"] != true {
		t.Fatalf("proxy-buffering config = %#v", plugins["proxy-buffering"])
	}
	if streamCase.Spec.Fixture.Response.Body != "" {
		t.Fatalf(
			"fixture body = %q: static completed bodies are not streaming evidence",
			streamCase.Spec.Fixture.Response.Body,
		)
	}
}

func TestDifferentialProxyBufferingStreamingCaseIsRegisteredWithoutWeakTransitCase(t *testing.T) {
	streamCase := differentialProxyBufferingStreamingCases()[0]
	found := 0
	for _, spec := range differentialCases() {
		if spec.Plugin != "proxy-buffering" {
			continue
		}
		found++
		if !reflect.DeepEqual(spec, streamCase.Spec) {
			t.Fatalf("registered proxy-buffering case = %#v, want streaming case", spec)
		}
	}
	if found != 1 {
		t.Fatalf("registered proxy-buffering cases = %d, want exactly the streaming case", found)
	}
	if _, ok := differentialProtocolDriverRegistry[differentialProxyBufferingSSEPolicy]; !ok {
		t.Fatalf("protocol driver %q is not registered", differentialProxyBufferingSSEPolicy)
	}
}
