package pluginintegration

import (
	"net/http"
	"strings"
)

// differentialProxyControlCases maps APISIX 3.17
// t/plugin/proxy-control.t TEST 1-2. The source block distinguishes buffering
// through NGINX logs; the deterministic HTTP boundary available here proves
// that disabling request buffering still delivers the exact large body once.
func differentialProxyControlCases() []DifferentialCase {
	const routeID = "differential-proxy-control-buffering-off"
	return []DifferentialCase{{
		Name:    "proxy-control-request-buffering-off-large-body",
		Plugin:  "proxy-control",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/hello",
				"plugins": map[string]any{
					"proxy-control": map[string]any{
						"request_buffering": false,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodPost,
			Path:   "/hello",
			Host:   "gateway.example.test",
			Body:   strings.Repeat("12345", 10240),
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 1,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "hello world",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
