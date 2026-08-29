package pluginintegration

import "net/http"

const differentialLimitConnGlobalSharedCapacityPolicy = "limit-conn-global-shared-capacity"

// differentialLimitConnCases maps APISIX 3.17 t/plugin/limit-conn.t TEST 21-22.
// The source's conn=2/burst=1 global rule admits three of ten concurrent
// requests. Using two routes additionally proves that the global rule owns one
// shared admission counter rather than one counter per route.
func differentialLimitConnCases() []DifferentialCase {
	const (
		routeA = "differential-limit-conn-route-a"
		routeB = "differential-limit-conn-route-b"
	)
	batch := make([]DifferentialRequest, 0, 10)
	for index := range 10 {
		path := "/limit-a"
		if index%2 == 1 {
			path = "/limit-b"
		}
		batch = append(batch, DifferentialRequest{
			Method: http.MethodGet, Path: path, Host: "gateway.example.test",
		})
	}
	return []DifferentialCase{{
		Name:             "limit-conn-global-rule-shares-capacity-across-routes",
		Plugin:           "limit-conn",
		RouteID:          routeA,
		ComparisonPolicy: differentialLimitConnGlobalSharedCapacityPolicy,
		Config: map[string]any{
			"routes": []any{
				map[string]any{
					"id": routeA, "uri": "/limit-a", "upstream": differentialUpstream(),
				},
				map[string]any{
					"id": routeB, "uri": "/limit-b", "upstream": differentialUpstream(),
				},
			},
			"global_rules": []any{map[string]any{
				"id": "differential-limit-conn-global",
				"plugins": map[string]any{
					"limit-conn": map[string]any{
						"conn": 2, "burst": 1, "default_conn_delay": 0.1,
						"key": "remote_addr", "rejected_code": http.StatusServiceUnavailable,
					},
				},
			}},
		},
		Steps: []DifferentialStep{
			{ConcurrentRequests: batch, SecurityDecision: "mixed"},
			{
				Request: DifferentialRequest{
					Method: http.MethodGet, Path: "/limit-b", Host: "gateway.example.test",
				},
				SecurityDecision: "allow",
			},
		},
		Fixture: DifferentialFixture{
			Name:          "limit-conn-origin",
			ExpectedCalls: 4,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK, Headers: map[string]string{"Content-Type": "text/plain"},
				Body: "hello world", DelayMillis: 1500,
			},
		},
	}}
}
