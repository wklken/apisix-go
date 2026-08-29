package pluginintegration

import "net/http"

// differentialEchoCases maps APISIX 3.17 t/plugin/echo.t TEST 3 and TEST 4
// at compatibilityOracleSourceCommit to one executable differential case.
func differentialEchoCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "echo-replace-body-and-add-headers",
			Plugin:  "echo",
			RouteID: "differential-echo-replace-body-and-add-headers",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "differential-echo-replace-body-and-add-headers",
					"uri": "/hello",
					"plugins": map[string]any{
						"echo": map[string]any{
							"before_body": "before the body modification ",
							"body":        "hello upstream",
							"after_body":  " after the body modification.",
							"headers": map[string]any{
								"Location":      "https://www.iresty.com",
								"Authorization": "userpass",
							},
						},
					},
					"upstream": map[string]any{
						"nodes": map[string]any{differentialFixturePlaceholder: 1},
						"type":  "roundrobin",
					},
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "hello world",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}
}
