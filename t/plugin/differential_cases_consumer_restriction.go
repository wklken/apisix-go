package pluginintegration

import "net/http"

// differentialConsumerRestrictionCases maps APISIX 3.17
// t/plugin/consumer-restriction.t TEST 3 through TEST 8 to standalone cases.
func differentialConsumerRestrictionCases() []DifferentialCase {
	newCase := func(name, routeID, authorization string, expectedCalls int, securityDecision string) DifferentialCase {
		headers := map[string]string(nil)
		if authorization != "" {
			headers = map[string]string{"Authorization": authorization}
		}

		return DifferentialCase{
			Name:    name,
			Plugin:  "consumer-restriction",
			RouteID: routeID,
			Config: map[string]any{
				"consumers": []any{
					map[string]any{
						"username": "jack1",
						"plugins": map[string]any{"basic-auth": map[string]any{
							"username": "jack2019", "password": "123456",
						}},
					},
					map[string]any{
						"username": "jack2",
						"plugins": map[string]any{"basic-auth": map[string]any{
							"username": "jack2020", "password": "123456",
						}},
					},
				},
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"basic-auth": map[string]any{},
						"consumer-restriction": map[string]any{
							"whitelist": []any{"jack1"},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/hello",
				Host:    "gateway.example.test",
				Headers: headers,
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: expectedCalls,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "hello world",
				},
			},
			SecurityDecision: securityDecision,
		}
	}

	return []DifferentialCase{
		newCase(
			"consumer-restriction-basic-whitelist-missing-authorization",
			"diff-consumer-restriction-missing-auth",
			"",
			0,
			"deny",
		),
		newCase(
			"consumer-restriction-basic-whitelist-jack1-allowed",
			"diff-consumer-restriction-jack1-allow",
			"Basic amFjazIwMTk6MTIzNDU2",
			1,
			"allow",
		),
		newCase(
			"consumer-restriction-basic-whitelist-jack2-denied",
			"diff-consumer-restriction-jack2-deny",
			"Basic amFjazIwMjA6MTIzNDU2",
			0,
			"deny",
		),
	}
}
