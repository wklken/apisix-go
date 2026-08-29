package pluginintegration

import "net/http"

// differentialIPRestrictionCases maps APISIX 3.17
// t/plugin/ip-restriction.t TEST 7/8, TEST 12/13, and TEST 26/27 to
// standalone loopback cases. The harness reaches both gateways from
// 127.0.0.1, so these cases do not depend on trusted proxy configuration.
func differentialIPRestrictionCases() []DifferentialCase {
	newCase := func(
		name, routeID, listName, message string,
		expectedCalls int,
		securityDecision string,
	) DifferentialCase {
		pluginConfig := map[string]any{
			listName: []any{"127.0.0.0/24"},
		}
		if message != "" {
			pluginConfig["message"] = message
		}
		return DifferentialCase{
			Name:    name,
			Plugin:  "ip-restriction",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"ip-restriction": pluginConfig,
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
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
			"ip-restriction-loopback-cidr-whitelist-allows",
			"differential-ip-whitelist-allow",
			"whitelist",
			"",
			1,
			"allow",
		),
		newCase(
			"ip-restriction-loopback-cidr-blacklist-denies",
			"differential-ip-blacklist-deny",
			"blacklist",
			"",
			0,
			"deny",
		),
		newCase(
			"ip-restriction-loopback-blacklist-custom-message",
			"differential-ip-blacklist-message",
			"blacklist",
			"Do you want to do something bad?",
			0,
			"deny",
		),
	}
}
