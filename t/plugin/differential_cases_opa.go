package pluginintegration

import "net/http"

const differentialOPAFixtureDecisionPolicy = "opa-fixture-decision"

// differentialOPACases maps APISIX 3.17 t/plugin/opa.t TEST 10 to a local
// policy endpoint. It proves the OPA input request and string-reason denial
// mapping without claiming a live OPA deployment.
func differentialOPACases() []DifferentialCase {
	const routeID = "differential-opa-string-reason-denial"

	return []DifferentialCase{{
		Name:             "opa-denies-with-string-reason",
		Plugin:           "opa",
		RouteID:          routeID,
		ComparisonPolicy: differentialOPAFixtureDecisionPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":   routeID,
				"uris": []any{"/hello", "/test"},
				"plugins": map[string]any{
					"opa": map[string]any{
						"host":   "http://" + differentialFixturePlaceholder,
						"policy": "example",
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/test?test=abcd&user=carla",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"test-header": "only-for-test",
			},
		},
		Fixture: DifferentialFixture{
			Name:            "primary",
			ExpectedCalls:   1,
			SemanticHeaders: []string{"Content-Type"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    `{"result":{"allow":false,"reason":"Give you a string reason"}}`,
			},
		},
		SecurityDecision: "deny",
	}}
}
