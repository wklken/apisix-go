package pluginintegration

import "net/http"

func differentialBasicAuthCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "basic-auth-hide-credentials",
			Plugin:  "basic-auth",
			RouteID: "differential-basic-auth-hide-credentials",
			Config: map[string]any{
				"consumers": []any{map[string]any{
					"username": "foo",
					"plugins": map[string]any{"basic-auth": map[string]any{
						"username": "foo", "password": "bar",
					}},
				}},
				"routes": []any{map[string]any{
					"id": "differential-basic-auth-hide-credentials", "uri": "/echo",
					"plugins": map[string]any{"basic-auth": map[string]any{
						"hide_credentials": true,
					}},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/echo", Host: "gateway.example.test",
				Headers: map[string]string{"Authorization": "Basic Zm9vOmJhcg=="},
			},
			Fixture: DifferentialFixture{
				Name: "primary", ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world"},
			},
			SecurityDecision: "allow",
		},
		{
			Name:    "basic-auth-preserve-credentials",
			Plugin:  "basic-auth",
			RouteID: "differential-basic-auth-preserve-credentials",
			Config: map[string]any{
				"consumers": []any{map[string]any{
					"username": "foo",
					"plugins": map[string]any{"basic-auth": map[string]any{
						"username": "foo", "password": "bar",
					}},
				}},
				"routes": []any{map[string]any{
					"id": "differential-basic-auth-preserve-credentials", "uri": "/echo",
					"plugins": map[string]any{"basic-auth": map[string]any{
						"hide_credentials": false,
					}},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/echo", Host: "gateway.example.test",
				Headers: map[string]string{"Authorization": "Basic Zm9vOmJhcg=="},
			},
			Fixture: DifferentialFixture{
				Name: "primary", ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "hello world"},
			},
			SecurityDecision: "allow",
		},
	}
}
