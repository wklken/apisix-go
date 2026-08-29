package pluginintegration

import "net/http"

// differentialJWTAuthCases maps APISIX 3.17 t/plugin/jwt-auth.t TEST 53
// through TEST 56 to its fixed HS256 token without an exp claim.
func differentialJWTAuthCases() []DifferentialCase {
	const token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJrZXkiOiJ1c2VyLWtleSJ9._7aoTZdzQDT0r9swHTcHb3nsujexcGjSTU-LRzTRVyY"
	return []DifferentialCase{{
		Name:    "jwt-auth-hs256-token-without-exp",
		Plugin:  "jwt-auth",
		RouteID: "diff-jwt-auth-hs256-no-exp",
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "jack",
				"plugins": map[string]any{"jwt-auth": map[string]any{
					"key": "user-key", "secret": "my-secret-key", "algorithm": "HS256",
				}},
			}},
			"routes": []any{map[string]any{
				"id": "diff-jwt-auth-hs256-no-exp", "uri": "/jwt",
				"plugins":  map[string]any{"jwt-auth": map[string]any{}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/jwt?jwt=" + token, Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "authorized"},
		},
		SecurityDecision: "allow",
	}}
}
