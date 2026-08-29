package pluginintegration

import "net/http"

// differentialMultiAuthCases maps APISIX 3.17 multi-auth.t TEST 1/2/4 and the
// pinned multi-auth first-success loop. Both credentials are valid for distinct
// consumers, so the upstream X-Consumer-Username proves Basic wins by order.
func differentialMultiAuthCases() []DifferentialCase {
	const routeID = "differential-multi-auth-basic-first"

	return []DifferentialCase{
		{
			Name:    "multi-auth-basic-wins-over-valid-key",
			Plugin:  "multi-auth",
			RouteID: routeID,
			Config: map[string]any{
				"consumers": []any{
					map[string]any{
						"username": "basic-user",
						"plugins": map[string]any{
							"basic-auth": map[string]any{
								"username": "foo",
								"password": "bar",
							},
						},
					},
					map[string]any{
						"username": "key-user",
						"plugins": map[string]any{
							"key-auth": map[string]any{"key": "auth-one"},
						},
					},
				},
				"routes": []any{map[string]any{
					"id": routeID, "uri": "/hello",
					"plugins": map[string]any{
						"multi-auth": map[string]any{
							"auth_plugins": []any{
								map[string]any{"basic-auth": map[string]any{}},
								map[string]any{"key-auth": map[string]any{
									"query": "apikey", "header": "apikey",
								}},
							},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"Authorization": "Basic Zm9vOmJhcg==",
					"apikey":        "auth-one",
				},
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "hello world",
				},
			},
			SecurityDecision: "allow",
		},
	}
}
