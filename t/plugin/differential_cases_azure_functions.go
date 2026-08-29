package pluginintegration

import "net/http"

const differentialAzureFunctionsFixtureInvocationPolicy = "azure-functions-fixture-invocation"

// differentialAzureFunctionsCases maps APISIX 3.17
// t/plugin/azure-functions.t TEST 4 and TEST 7 to a local HTTP function. It
// proves response forwarding and configured function-key injection without
// claiming a live Azure Functions service.
func differentialAzureFunctionsCases() []DifferentialCase {
	const routeID = "differential-azure-functions-keyed-invocation"

	return []DifferentialCase{{
		Name:             "azure-functions-keyed-local-invocation",
		Plugin:           "azure-functions",
		RouteID:          routeID,
		ComparisonPolicy: differentialAzureFunctionsFixtureInvocationPolicy,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id": routeID, "uri": "/azure",
					"plugins": map[string]any{
						"azure-functions": map[string]any{
							"function_uri": "http://" + differentialFixturePlaceholder + "/httptrigger",
							"authorization": map[string]any{
								"apikey": "test_key",
							},
						},
					},
				},
			},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/azure",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:            "primary",
			ExpectedCalls:   1,
			SemanticHeaders: []string{"X-Functions-Key"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"X-Extra-Header": "MUST"},
				Body:    "faas invoked",
			},
		},
		SecurityDecision: "allow",
	}}
}
