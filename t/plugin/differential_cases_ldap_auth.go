package pluginintegration

import "net/http"

// differentialLDAPAuthCases maps APISIX 3.17 t/plugin/ldap-auth.t TEST 3-5.
// Missing credentials are rejected before any LDAP or upstream network call.
func differentialLDAPAuthCases() []DifferentialCase {
	const routeID = "differential-ldap-auth-missing-authorization"
	return []DifferentialCase{{
		Name:    "ldap-auth-missing-authorization",
		Plugin:  "ldap-auth",
		RouteID: routeID,
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "user01",
				"plugins": map[string]any{"ldap-auth": map[string]any{
					"user_dn": "cn=user01,ou=users,dc=example,dc=org",
				}},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{"ldap-auth": map[string]any{
					"base_dn":  "ou=users,dc=example,dc=org",
					"ldap_uri": differentialFixturePlaceholder,
					"uid":      "cn",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}
