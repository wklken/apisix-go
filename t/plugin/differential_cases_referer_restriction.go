package pluginintegration

import "net/http"

// differentialRefererRestrictionCases maps APISIX 3.17
// t/plugin/referer-restriction.t TEST 1/2 and TEST 4 to standalone cases.
func differentialRefererRestrictionCases() []DifferentialCase {
	newCase := func(name, routeID, referer string, expectedCalls int, securityDecision string) DifferentialCase {
		return DifferentialCase{
			Name:    name,
			Plugin:  "referer-restriction",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"referer-restriction": map[string]any{
							"whitelist": []any{"*.xx.com", "yy.com"},
						},
					},
					"upstream": map[string]any{
						"type":  "roundrobin",
						"nodes": map[string]any{differentialFixturePlaceholder: 1},
					},
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/hello",
				Host:    "gateway.example.test",
				Headers: map[string]string{"Referer": referer},
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
			"referer-restriction-wildcard-whitelist-allows",
			"differential-referer-wildcard-allow",
			"http://www.xx.com",
			1,
			"allow",
		),
		newCase(
			"referer-restriction-exact-whitelist-rejects-subdomain",
			"differential-referer-exact-deny",
			"https://www.yy.com/am",
			0,
			"deny",
		),
	}
}
