package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialGRPCTranscodeCaseMapsPinnedAPISIX317UnaryGET(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialGRPCTranscodeCases()
	if len(cases) != 1 {
		t.Fatalf("differentialGRPCTranscodeCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "grpc-transcode-unary-get" || spec.Plugin != "grpc-transcode" ||
		spec.RouteID != "differential-grpc-transcode-unary-get" {
		t.Fatalf("case identity = %q/%q/%q", spec.Name, spec.Plugin, spec.RouteID)
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/grpctest?name=world" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.ComparisonPolicy != differentialGRPCTranscodeUnaryPolicy ||
		spec.SecurityDecision != "not_applicable" {
		t.Fatalf("policy/decision = %q/%q", spec.ComparisonPolicy, spec.SecurityDecision)
	}
	if spec.Fixture.Name != "grpc" || spec.Fixture.WireProtocol != differentialFixtureWireGRPCH2C ||
		spec.Fixture.ExpectedCalls != 1 || spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Body != "CgtIZWxsbyB3b3JsZA==" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if len(spec.Fixture.SemanticHeaders) != 1 || spec.Fixture.SemanticHeaders[0] != "Content-Type" {
		t.Fatalf("fixture semantic headers = %#v", spec.Fixture.SemanticHeaders)
	}

	protos, ok := spec.Config["protos"].([]any)
	if !ok || len(protos) != 1 {
		t.Fatalf("protos = %#v", spec.Config["protos"])
	}
	protoConfig, ok := protos[0].(map[string]any)
	if !ok || protoConfig["id"] != "1" || protoConfig["content"] != differentialGRPCTranscodeHelloProto {
		t.Fatalf("proto config = %#v", protos[0])
	}
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	if route["id"] != spec.RouteID || route["uri"] != "/grpctest" {
		t.Fatalf("route identity = %#v", route)
	}
	plugins := route["plugins"].(map[string]any)
	pluginConfig := plugins["grpc-transcode"].(map[string]any)
	if pluginConfig["proto_id"] != "1" || pluginConfig["service"] != "helloworld.Greeter" ||
		pluginConfig["method"] != "SayHello" {
		t.Fatalf("plugin config = %#v", pluginConfig)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["scheme"] != "grpc" {
		t.Fatalf("upstream = %#v", upstream)
	}
	nodes := upstream["nodes"].(map[string]any)
	if nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", nodes)
	}
}
