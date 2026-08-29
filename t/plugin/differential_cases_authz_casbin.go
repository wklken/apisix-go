package pluginintegration

import "net/http"

const differentialCasbinModel = `[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[role_definition]
g = _, _

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = (g(r.sub, p.sub) || keyMatch(r.sub, p.sub)) && keyMatch(r.obj, p.obj) && keyMatch(r.act, p.act)`

// differentialAuthzCasbinCases maps APISIX 3.17
// t/plugin/authz-casbin.t TEST 3-6. Metadata makes the username-only route
// executable; omitting the configured user header must stop before upstream.
func differentialAuthzCasbinCases() []DifferentialCase {
	const routeID = "differential-authz-casbin-missing-username"
	return []DifferentialCase{{
		Name:    "authz-casbin-missing-username",
		Plugin:  "authz-casbin",
		RouteID: routeID,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id": "authz-casbin", "model": differentialCasbinModel,
				"policy": "p, *, /, GET\np, admin, *, *\ng, alice, admin",
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{
					"authz-casbin": map[string]any{"username": "user"},
				},
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
