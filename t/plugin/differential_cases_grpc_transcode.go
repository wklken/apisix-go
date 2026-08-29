package pluginintegration

import "net/http"

const differentialGRPCTranscodeHelloProto = `syntax = "proto3";
package helloworld;
service Greeter {
  rpc SayHello (HelloRequest) returns (HelloReply) {}
}
message HelloRequest {
  string name = 1;
}
message HelloReply {
  string message = 1;
}`

// differentialGRPCTranscodeCases maps APISIX 3.17
// t/plugin/grpc-transcode.t TEST 5 to one real unary h2c exchange.
func differentialGRPCTranscodeCases() []DifferentialCase {
	const routeID = "differential-grpc-transcode-unary-get"
	return []DifferentialCase{{
		Name:             "grpc-transcode-unary-get",
		Plugin:           "grpc-transcode",
		RouteID:          routeID,
		ComparisonPolicy: differentialGRPCTranscodeUnaryPolicy,
		Config: map[string]any{
			"protos": []any{map[string]any{
				"id": "1", "content": differentialGRPCTranscodeHelloProto,
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/grpctest", "methods": []any{"GET", "POST"},
				"plugins": map[string]any{"grpc-transcode": map[string]any{
					"proto_id": "1", "service": "helloworld.Greeter", "method": "SayHello",
				}},
				"upstream": map[string]any{
					"scheme": "grpc", "type": "roundrobin",
					"nodes": map[string]any{differentialFixturePlaceholder: 1},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/grpctest?name=world", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "grpc", WireProtocol: differentialFixtureWireGRPCH2C, ExpectedCalls: 1,
			SemanticHeaders: []string{"Content-Type"},
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK, Body: "CgtIZWxsbyB3b3JsZA==",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
