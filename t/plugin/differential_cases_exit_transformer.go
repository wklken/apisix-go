package pluginintegration

import "net/http"

const differentialExitTransformerMissingAPIKeyFunction = `return
    (function(code, body, header)
        if code == 401 and body.message == "Missing API key in request" then
            return 400, {message = "authentication Failed"}, {["content-type"] = "application/json"}
        end
        return code, body, header
    end)
(...)`

// differentialExitTransformerCases maps APISIX 3.17
// t/plugin/exit-transformer.t TEST 17 and TEST 19. A missing key-auth
// credential is transformed into the source block's exact status and JSON body.
func differentialExitTransformerCases() []DifferentialCase {
	const routeID = "differential-exit-transformer-missing-api-key"

	return []DifferentialCase{{
		Name:    "exit-transformer-missing-api-key-json",
		Plugin:  "exit-transformer",
		RouteID: routeID,
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "jack",
				"plugins": map[string]any{
					"key-auth": map[string]any{"key": "auth-one"},
				},
			}},
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"key-auth": map[string]any{},
					"exit-transformer": map[string]any{
						"functions": []any{differentialExitTransformerMissingAPIKeyFunction},
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
				Body:   "unused",
			},
		},
		SecurityDecision: "deny",
	}}
}
