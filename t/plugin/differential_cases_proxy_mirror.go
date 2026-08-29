package pluginintegration

import "net/http"

// differentialProxyMirrorCases maps APISIX 3.17 t/plugin/proxy-mirror.t
// TEST 6/7. A single client request must reach the normal upstream and also
// produce one best-effort mirror request with the original path.
func differentialProxyMirrorCases() []DifferentialCase {
	const routeID = "differential-proxy-mirror-normal"

	return []DifferentialCase{{
		Name:    "proxy-mirror-normal-delivery",
		Plugin:  "proxy-mirror",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"proxy-mirror": map[string]any{
						"host": "http://" + differentialFixturePlaceholder,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/hello",
				Host:   "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name:          "primary-and-mirror",
			ExpectedCalls: 2,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world\n",
			},
		},
	}}
}
