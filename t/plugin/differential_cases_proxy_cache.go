package pluginintegration

import "net/http"

// differentialProxyCacheCases maps APISIX 3.17
// t/plugin/proxy-cache/memory.t TEST 3 and TEST 4 to the first cacheable
// request. The exact comparator observes the plugin-owned MISS response header.
func differentialProxyCacheCases() []DifferentialCase {
	const routeID = "differential-proxy-cache-memory-miss"

	return []DifferentialCase{{
		Name:    "proxy-cache-memory-first-request-miss",
		Plugin:  "proxy-cache",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"proxy-cache": map[string]any{
						"cache_strategy":     "memory",
						"cache_key":          []any{"$host", "$uri"},
						"cache_zone":         "memory_cache",
						"cache_bypass":       []any{"$arg_bypass"},
						"cache_method":       []any{http.MethodGet},
						"hide_cache_headers": false,
						"cache_ttl":          300,
						"cache_http_status":  []any{http.StatusOK},
						"no_cache":           []any{"$arg_no_cache"},
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
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world!",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
