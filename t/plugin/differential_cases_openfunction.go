package pluginintegration

import "net/http"

// differentialOpenFunctionCases maps APISIX 3.17 openfunction.t TEST 6/7
// to a local HTTP function. It proves request and response mapping only and
// makes no claim about a live OpenFunction installation.
func differentialOpenFunctionCases() []DifferentialCase {
	const routeID = "differential-openfunction-post-body"

	return []DifferentialCase{
		{
			Name:    "openfunction-post-body-response",
			Plugin:  "openfunction",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/hello",
					"plugins": map[string]any{
						"openfunction": map[string]any{
							"function_uri": "http://" + differentialFixturePlaceholder + "/default/test-body",
						},
					},
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/hello",
				Host:   "gateway.example.test",
				Body:   "test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "Hello, test!",
				},
			},
			SecurityDecision: "allow",
		},
	}
}
