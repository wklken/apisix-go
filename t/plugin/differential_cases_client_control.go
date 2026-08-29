package pluginintegration

import "net/http"

const (
	// Keep the rejection case name stable: APISIX's NGINX-generated 413 body
	// differs from the Go response template. A reviewed comparison policy can
	// key its body-only exception to this case without weakening other cases or
	// the status/no-upstream assertions.
	differentialClientControlContentLengthTooLargeCase   = "client-control-content-length-too-large"
	differentialClientControlContentLengthExactLimitCase = "client-control-content-length-exact-limit"
)

// differentialClientControlCases maps APISIX 3.17
// t/plugin/client-control.t TEST 1-4 to standalone Content-Length cases.
// The chunked-body blocks are deliberately outside this batch.
func differentialClientControlCases() []DifferentialCase {
	newCase := func(name, routeID, body string, expectedCalls int, securityDecision string) DifferentialCase {
		return DifferentialCase{
			Name:    name,
			Plugin:  "client-control",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"client-control": map[string]any{"max_body_size": 5},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/hello",
				Host:   "gateway.example.test",
				Body:   body,
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: expectedCalls,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "done",
				},
			},
			SecurityDecision: securityDecision,
		}
	}

	cases := []DifferentialCase{
		newCase(
			differentialClientControlContentLengthTooLargeCase,
			"differential-client-control-too-large",
			"123456",
			0,
			"deny",
		),
		newCase(
			differentialClientControlContentLengthExactLimitCase,
			"differential-client-control-exact-limit",
			"12345",
			1,
			"allow",
		),
	}
	cases[0].ComparisonPolicy = differentialComparisonPlatformOwnedErrorRepresentation
	return cases
}
