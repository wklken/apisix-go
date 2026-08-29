package pluginintegration

import "net/http"

const differentialComparisonNodeStatusJSONCounters = "node-status-json-counters"

// differentialNodeStatusCases maps APISIX 3.17 t/plugin/node-status.t TEST 1/2.
// The source exposes the plugin-owned GET API through public-api and verifies
// that the response contains the node ID plus request counters. The comparator
// must validate those JSON semantics without requiring process-local values or
// NGINX-only reading/writing/waiting counters to match.
func differentialNodeStatusCases() []DifferentialCase {
	const routeID = "differential-node-status-public-api"

	return []DifferentialCase{{
		Name:             "node-status-reports-json-counters",
		Plugin:           "node-status",
		RouteID:          routeID,
		ComparisonPolicy: differentialComparisonNodeStatusJSONCounters,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":  routeID,
				"uri": "/apisix/status",
				"plugins": map[string]any{
					"public-api": map[string]any{},
				},
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodGet,
			Path:   "/apisix/status",
			Host:   "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name:          "primary",
			ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK,
				Body:   "unused",
			},
		},
		SecurityDecision: "not_applicable",
	}}
}
