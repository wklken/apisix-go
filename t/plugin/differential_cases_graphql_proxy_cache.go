package pluginintegration

import "net/http"

// differentialGraphQLProxyCacheCases maps APISIX 3.17
// t/plugin/graphql-proxy-cache/graphql.t TEST 4 to the wrong-method obligation.
// HEAD is rejected with 405 before the origin is called.
func differentialGraphQLProxyCacheCases() []DifferentialCase {
	const routeID = "diff-graphql-proxy-cache-wrong-method"

	return []DifferentialCase{{
		Name:    "graphql-proxy-cache-wrong-method-head",
		Plugin:  "graphql-proxy-cache",
		RouteID: routeID,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id":       routeID,
				"uri":      "/graphql",
				"plugins":  map[string]any{"graphql-proxy-cache": map[string]any{}},
				"upstream": differentialUpstream(),
			}},
		},
		Request: DifferentialRequest{
			Method: http.MethodHead, Path: "/graphql", Host: "gateway.example.test",
		},
		Fixture: DifferentialFixture{
			Name: "origin", ExpectedCalls: 0,
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "unused"},
		},
		ComparisonPolicy: differentialComparisonGraphQLHeadErrorContentType,
		SecurityDecision: "not_applicable",
	}}
}
