package pluginintegration

import "net/http"

// differentialServerlessPostFunctionCases intentionally maps only the
// reviewed APISIX 3.17 serverless.t TEST 25/26 early-stop contract. It does
// not claim general Lua, OpenResty phase, or streaming parity.
func differentialServerlessPostFunctionCases() []DifferentialCase {
	const routeID = "differential-serverless-post-forbidden"

	return []DifferentialCase{
		{
			Name:    "serverless-post-function-early-forbidden",
			Plugin:  "serverless-post-function",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/hello",
					"plugins": map[string]any{
						"serverless-post-function": map[string]any{
							"functions": []any{
								"return function(conf, ctx) return 403, 'forbidden' end",
								"return function(conf, ctx) ngx.log(ngx.ERR, 'unreachable') end",
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
			SecurityDecision: "deny",
		},
	}
}
