package pluginintegration

import "net/http"

// differentialExamplePluginCases maps APISIX 3.17 t/plugin/example.t TEST 8/9
// at compatibilityOracleSourceCommit. The split fixture placeholders preserve
// the plugin's string host and integer port schema while exercising its route
// upstream override against the single primary fixture.
func differentialExamplePluginCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "example-plugin-overrides-route-upstream",
			Plugin:  "example-plugin",
			RouteID: "diff-example-plugin-upstream",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-example-plugin-upstream",
					"uri": "/server_port",
					"plugins": map[string]any{
						"example-plugin": map[string]any{
							"i":    11,
							"ip":   differentialFixtureHostPlaceholder,
							"port": differentialFixturePortPlaceholder,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/server_port",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "example-plugin",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}
}
