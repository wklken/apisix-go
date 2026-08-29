package pluginintegration

import "net/http"

// differentialServerlessPreFunctionCases maps APISIX 3.17 serverless.t
// TEST 9/10. It intentionally proves only the bounded ngx.exit behavior that
// this Go-native implementation supports, not general Lua/OpenResty parity.
func differentialServerlessPreFunctionCases() []DifferentialCase {
	const routeID = "differential-serverless-pre-exit"

	return []DifferentialCase{{
		Name:    "serverless-pre-function-exits-201",
		Plugin:  "serverless-pre-function",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{
					"serverless-pre-function": map[string]any{
						"functions": []any{
							"return function() ngx.log(ngx.ERR, 'serverless pre function'); ngx.exit(201); end",
						},
					},
				},
				"upstream": differentialUpstream(),
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
				Body:   "unexpected",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
