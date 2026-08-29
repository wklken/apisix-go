package pluginintegration

import "net/http"

// differentialBatchRequestsCases maps APISIX 3.17
// t/plugin/batch-requests.t TEST 1 and TEST 3. APISIX exposes the internal
// batch API through a public-api route without an upstream; the malformed
// request must then return 400 before batch dispatch reaches the fixture.
func differentialBatchRequestsCases() []DifferentialCase {
	const routeID = "differential-batch-requests-missing-pipeline"

	return []DifferentialCase{{
		Name:    "batch-requests-missing-pipeline",
		Plugin:  "batch-requests",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/apisix/batch-requests",
				"plugins": map[string]any{
					"public-api": map[string]any{},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/apisix/batch-requests",
			Host:   "gateway.example.test",
			Body:   `{"pipeline1":[{"path":"/b"}]}`,
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
