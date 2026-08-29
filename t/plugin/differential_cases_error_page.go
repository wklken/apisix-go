package pluginintegration

import "net/http"

// differentialErrorPageCases maps APISIX 3.17 t/plugin/error-page.t TEST 4
// and TEST 5 to a self-contained early 500 response. The bounded serverless
// function replaces the source block's ngx.exit trigger; error-page still owns
// the observed metadata-selected body and content type.
func differentialErrorPageCases() []DifferentialCase {
	const routeID = "differential-error-page-custom-500"

	return []DifferentialCase{{
		Name:             "error-page-custom-500-body",
		Plugin:           "error-page",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonErrorPageCharsetParameter,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"serverless-post-function": map[string]any{
						"functions": []any{
							"return function(conf, ctx) return 500, 'openresty' end",
						},
					},
				},
				"upstream": differentialUpstream(),
			}},
			"global_rules": []any{map[string]any{
				"id": "differential-error-page-global",
				"plugins": map[string]any{
					"error-page": map[string]any{},
				},
			}},
			"plugin_metadata": []any{map[string]any{
				"id":     "error-page",
				"enable": true,
				"error_500": map[string]any{
					"body": "<html><body><h1>500 Internal Server Error</h1></body></html>",
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/hello",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unused",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
