package pluginintegration

import "net/http"

// differentialOpenWhiskCases maps APISIX 3.17 openwhisk.t TEST 8/9 to a
// local HTTP action endpoint. It covers action URL, Basic authorization, JSON
// body, and action response mapping without claiming a live OpenWhisk system.
func differentialOpenWhiskCases() []DifferentialCase {
	const routeID = "differential-openwhisk-json-action"

	return []DifferentialCase{
		{
			Name:             "openwhisk-json-action-response",
			Plugin:           "openwhisk",
			RouteID:          routeID,
			ComparisonPolicy: differentialComparisonFixtureOwnedUpstreamEndpoint,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/hello",
					"plugins": map[string]any{
						"openwhisk": map[string]any{
							"api_host":      "http://" + differentialFixturePlaceholder,
							"service_token": "test:test",
							"namespace":     "guest",
							"action":        "test-params",
							"result":        true,
						},
					},
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/hello",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
				Body: `{"name":"world"}`,
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   `{"statusCode":200,"headers":{"Content-Type":"application/json"},"body":"{\"hello\":\"world\"}"}`,
				},
			},
			SecurityDecision: "allow",
		},
	}
}
