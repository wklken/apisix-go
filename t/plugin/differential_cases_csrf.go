package pluginintegration

import "net/http"

// CSRF signs the response cookie with a random value and the current time.
// The comparison layer must normalize only those dynamic cookie fields while
// retaining cookie presence, name, attributes, status, body, and upstream use.
const differentialCSRFIssuedCookieComparisonPolicy = "csrf-issued-cookie"

// differentialCSRFCases maps APISIX 3.17 t/plugin/csrf.t TEST 2-4:
// TEST 2 installs the route, TEST 3 proves a safe GET reaches upstream and
// issues the CSRF cookie, and TEST 4 rejects a POST without a token.
func differentialCSRFCases() []DifferentialCase {
	newCase := func(
		name, routeID, method string,
		expectedCalls int,
		securityDecision string,
	) DifferentialCase {
		return DifferentialCase{
			Name:             name,
			Plugin:           "csrf",
			RouteID:          routeID,
			ComparisonPolicy: differentialCSRFIssuedCookieComparisonPolicy,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"csrf": map[string]any{
							"key":     "userkey",
							"expires": 1000000000,
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: method,
				Path:   "/hello",
				Host:   "gateway.example.test",
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
			"csrf-safe-get-issues-cookie",
			"differential-csrf-safe-get",
			http.MethodGet,
			1,
			"allow",
		),
		newCase(
			"csrf-post-missing-token-rejected",
			"differential-csrf-missing-token",
			http.MethodPost,
			0,
			"deny",
		),
	}
}
