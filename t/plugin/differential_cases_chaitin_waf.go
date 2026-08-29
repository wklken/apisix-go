package pluginintegration

import "net/http"

// differentialChaitinWAFCases maps APISIX 3.17
// t/plugin/chaitin-waf-reject.t TEST 1/2. The local WAF fixture rejects the
// request in block mode, so the route upstream must not be called.
func differentialChaitinWAFCases() []DifferentialCase {
	const routeID = "differential-chaitin-waf-reject"

	return []DifferentialCase{{
		Name:             "chaitin-waf-block-mode-reject",
		Plugin:           "chaitin-waf",
		RouteID:          routeID,
		ComparisonPolicy: "chaitin-waf-elapsed-time",
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id":   "chaitin-waf",
				"mode": "block",
				"nodes": []any{map[string]any{
					"host": differentialFixtureHostPlaceholder,
					"port": differentialFixturePortPlaceholder,
				}},
			}},
			"routes": []any{map[string]any{
				"id":      routeID,
				"uri":     "/hello",
				"methods": []any{http.MethodGet},
				"plugins": map[string]any{
					"chaitin-waf": map[string]any{
						"mode": "block",
						"upstream": map[string]any{
							"servers": []any{"httpbun.org"},
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
		},
		Fixture: DifferentialFixture{
			Name:          "waf",
			WireProtocol:  differentialFixtureWireT1KV2,
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusForbidden,
				Body:   `{"status":403,"event_id":"b3c6ce574dc24f09a01f634a39dca83b"}`,
			},
		},
		SecurityDecision: "deny",
	}}
}
