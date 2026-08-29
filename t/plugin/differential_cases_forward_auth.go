package pluginintegration

import "net/http"

// differentialForwardAuthCases maps APISIX 3.17
// t/plugin/forward-auth.t TEST 2/5. The local fixture replaces the source
// auth subrequest route and returns the same denied status and client header.
func differentialForwardAuthCases() []DifferentialCase {
	const routeID = "differential-forward-auth-client-header"
	return []DifferentialCase{{
		Name:             "forward-auth-deny-copies-client-header",
		Plugin:           "forward-auth",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonForwardAuthEmptyErrorContentType,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{"forward-auth": map[string]any{
					"uri":             "http://" + differentialFixturePlaceholder + "/auth",
					"request_headers": []any{"Authorization"},
					"client_headers":  []any{"Location"},
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet, Path: "/hello", Host: "gateway.example.test",
			Headers: map[string]string{"Authorization": "333"},
		},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status:  http.StatusForbidden,
				Headers: map[string]string{"Location": "http://example.com/auth"},
			},
		},
		SecurityDecision: "deny",
	}}
}
