package pluginintegration

import "net/http"

// differentialGraphQLLimitCountCases maps APISIX 3.17
// t/plugin/graphql-limit-count.t TEST 3 to the wrong-method obligation.
// HEAD is rejected with 405 before the upstream is called.
func differentialGraphQLLimitCountCases() []DifferentialCase {
	const routeID = "diff-graphql-limit-count-wrong-method"

	return []DifferentialCase{{
		Name:    "graphql-limit-count-wrong-method-head",
		Plugin:  "graphql-limit-count",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/hello",
				"plugins": map[string]any{"graphql-limit-count": map[string]any{
					"count": 4, "time_window": 60, "rejected_code": 503, "key": "remote_addr",
				}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodHead, Path: "/hello", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "upstream", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		SecurityDecision: "not_applicable",
	}}
}
