package pluginintegration

import "net/http"

// differentialProxyBufferingCases maps the deterministic transit boundary of
// APISIX 3.17 t/cli/test_proxy_buffering.sh: enable disable_proxy_buffering for
// an SSE request and preserve the event stream response. The pinned source's
// decisive assertion is stronger: three separately flushed frames must arrive
// while the upstream connection remains open, over HTTP and HTTPS. The shared
// fixture currently writes one static response and observes only the completed
// body, so this case deliberately does not claim chunk-arrival/timing parity.
func differentialProxyBufferingCases() []DifferentialCase {
	const routeID = "differential-proxy-buffering-disabled"
	return []DifferentialCase{{
		Name:    "proxy-buffering-disabled-sse-transit",
		Plugin:  "proxy-buffering",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/events",
				"plugins": map[string]any{
					"proxy-buffering": map[string]any{
						"disable_proxy_buffering": true,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/events",
			Host:   "gateway.example.test",
			Headers: map[string]string{
				"Accept": "text/event-stream",
			},
		},
		Fixture: DifferentialFixture{
			Name:            "primary",
			ExpectedCalls:   1,
			SemanticHeaders: []string{"Accept"},
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"Content-Type": "text/event-stream",
				},
				Body: "data: event-1\n\ndata: event-2\n\ndata: event-3\n\n",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
