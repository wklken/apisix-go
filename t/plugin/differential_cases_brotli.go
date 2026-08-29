package pluginintegration

import "net/http"

// differentialBrotliCases maps APISIX 3.17 t/plugin/brotli.t TEST 1/2.
// DisableCompression keeps Content-Encoding and the compressed wire body in
// the observation; compressed-response-semantics must decode that body before
// comparing the plugin-owned response semantics.
func differentialBrotliCases() []DifferentialCase {
	const (
		routeID = "differential-brotli-default-compression"
		body    = "0123456789\n012345678"
	)

	return []DifferentialCase{{
		Name:             "brotli-default-compression",
		Plugin:           "brotli",
		RouteID:          routeID,
		ComparisonPolicy: "compressed-response-semantics",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/echo",
				"plugins": map[string]any{
					"brotli": map[string]any{},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/echo",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Accept-Encoding": "br",
				"Content-Type":    "text/html",
			},
			Body: body,
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "text/html",
				},
				Body: body,
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
