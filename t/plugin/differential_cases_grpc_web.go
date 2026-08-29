package pluginintegration

import "net/http"

// differentialGRPCWebCases maps APISIX 3.17 t/plugin/grpc-web.t TEST 1/4.
// The OPTIONS preflight is answered by grpc-web before the gRPC upstream.
func differentialGRPCWebCases() []DifferentialCase {
	const routeID = "differential-grpc-web-options"

	return []DifferentialCase{{
		Name:    "grpc-web-options-preflight",
		Plugin:  "grpc-web",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/grpc/web/*",
				"plugins": map[string]any{
					"grpc-web": map[string]any{},
				},
				"upstream": map[string]any{
					"scheme": "grpc",
					"type":   "roundrobin",
					"nodes":  map[string]any{differentialFixturePlaceholder: 1},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodOptions,
			Path:   "/grpc/web/a6.RouteService/GetRoute",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unexpected",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
