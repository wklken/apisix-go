package pluginintegration

import "net/http"

// differentialAWSLambdaCases maps APISIX 3.17 t/plugin/aws-lambda.t TEST 3/4
// to a local HTTP function endpoint. It covers invocation and response
// forwarding without claiming live AWS, API-key, or IAM evidence.
func differentialAWSLambdaCases() []DifferentialCase {
	const routeID = "differential-aws-lambda-local-function"

	return []DifferentialCase{{
		Name:             "aws-lambda-local-function-response",
		Plugin:           "aws-lambda",
		RouteID:          routeID,
		ComparisonPolicy: "fixture-owned-function-endpoint",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/aws",
				"plugins": map[string]any{
					"aws-lambda": map[string]any{
						"function_uri": "http://" + differentialFixturePlaceholder + "/httptrigger",
					},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/aws",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "aws lambda invoked",
			},
		},
		SecurityDecision: "allow",
	}}
}
