package pluginintegration

import "net/http"

// differentialRealIPCases combines APISIX 3.17 t/plugin/real-ip.t TEST 2/3's
// http_xff source mapping with TEST 18/19 and TEST 20/21's trusted-peer
// boundary. Both cases send the same address from the harness's loopback peer,
// so their only behavioral difference is whether the plugin trusts that peer.
func differentialRealIPCases() []DifferentialCase {
	newCase := func(
		name, routeID string,
		trustedAddresses []any,
		expectedCalls int,
		securityDecision string,
	) DifferentialCase {
		return DifferentialCase{
			Name:    name,
			Plugin:  "real-ip",
			RouteID: routeID,
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  routeID,
					"uri": "/hello",
					"plugins": map[string]any{
						"real-ip": map[string]any{
							"source":            "http_xff",
							"trusted_addresses": trustedAddresses,
						},
						"ip-restriction": map[string]any{
							"whitelist": []any{"1.1.1.1"},
						},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method:  http.MethodGet,
				Path:    "/hello",
				Host:    "gateway.example.test",
				Headers: map[string]string{"XFF": "1.1.1.1"},
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
			"real-ip-trusted-peer-rewrites-from-forwarded-for",
			"diff-real-ip-trusted-peer",
			[]any{"192.128.0.0/16", "127.0.0.0/24"},
			1,
			"allow",
		),
		newCase(
			"real-ip-untrusted-peer-ignores-forwarded-for",
			"diff-real-ip-untrusted-peer",
			[]any{"192.128.0.0/16"},
			0,
			"deny",
		),
	}
}
