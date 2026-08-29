package pluginintegration

import "net/http"

// differentialFaultInjectionCases maps APISIX 3.17
// t/plugin/fault-injection.t TEST 14 and TEST 15 to an exact fixed abort.
func differentialFaultInjectionCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "fault-injection-fixed-abort",
		Plugin:  "fault-injection",
		RouteID: "diff-fault-injection-fixed-abort",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": "diff-fault-injection-fixed-abort", "uri": "/fault",
				"plugins": map[string]any{"fault-injection": map[string]any{
					"abort": map[string]any{
						"http_status": 405,
						"body":        "Fault Injection!\n",
					},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/fault", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "not_applicable",
	}}
}
