package pluginintegration

import "net/http"

// differentialAuthzKeycloakCases maps APISIX 3.17
// t/plugin/authz-keycloak.t TEST 13/14. ENFORCING with its default empty
// permissions rejects before contacting Keycloak or the route upstream.
func differentialAuthzKeycloakCases() []DifferentialCase {
	const routeID = "differential-authz-keycloak-empty-permissions"
	return []DifferentialCase{{
		Name:    "authz-keycloak-enforcing-empty-permissions",
		Plugin:  "authz-keycloak",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello1",
				"plugins": map[string]any{"authz-keycloak": map[string]any{
					"token_endpoint":          "http://" + differentialFixturePlaceholder + "/token",
					"client_id":               "course_management",
					"grant_type":              "urn:ietf:params:oauth:grant-type:uma-ticket",
					"policy_enforcement_mode": "ENFORCING",
					"timeout":                 3000,
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello1", Host: "gateway.example.test",
			Headers: map[string]string{"Authorization": "Bearer fake access token"},
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unexpected"},
		},
		SecurityDecision: "deny",
	}}
}
