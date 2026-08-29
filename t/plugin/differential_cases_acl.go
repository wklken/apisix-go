package pluginintegration

import "net/http"

// differentialACLCases maps APISIX 3.17 t/plugin/acl.t TEST 2 and TEST 7
// through TEST 9 to a denied allow-label case with basic-auth as collaborator.
func differentialACLCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:    "acl-basic-auth-nonmatching-label-denied",
		Plugin:  "acl",
		RouteID: "diff-acl-basic-auth-label-deny",
		Config: map[string]any{
			"consumers": []any{map[string]any{
				"username": "rose",
				"plugins": map[string]any{"basic-auth": map[string]any{
					"username": "rose", "password": "123456",
				}},
				"labels": map[string]any{
					"project": `["tomcat","web-server","http,server"]`,
				},
			}},
			"routes": []any{map[string]any{
				"id": "diff-acl-basic-auth-label-deny", "uri": "/acl",
				"plugins": map[string]any{
					"basic-auth": map[string]any{},
					"acl": map[string]any{
						"allow_labels":  map[string]any{"project": []any{"apisix"}},
						"rejected_code": 403,
						"rejected_msg":  "The consumer is forbidden.",
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/acl", Host: "gateway.example.test",
			Headers: map[string]string{"Authorization": "Basic cm9zZToxMjM0NTY="},
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		SecurityDecision: "deny",
	}}
}
