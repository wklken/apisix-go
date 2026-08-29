package pluginintegration

import "net/http"

// differentialBodyTransformerCases maps APISIX 3.17
// t/plugin/body-transformer.t TEST 2 to an exact JSON request rewrite.
func differentialBodyTransformerCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "body-transformer-json-request-rewrite",
		Plugin:  "body-transformer",
		RouteID: "diff-body-transformer-json-request",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-body-transformer-json-request", "uri": "/transform",
				"plugins": map[string]any{"body-transformer": map[string]any{
					"request": map[string]any{
						"input_format": "json",
						"template":     `{"bar":{{age+10}}}`,
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost, Path: "/transform", Host: "gateway.example.test",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"age":20}`,
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "transformed"},
		},
		SecurityDecision: "not_applicable",
	}}
}
