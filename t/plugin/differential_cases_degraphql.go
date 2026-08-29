package pluginintegration

import "net/http"

const differentialDegraphqlQuery = "{persons{id}}"

// differentialDegraphqlCases maps APISIX 3.17 t/plugin/degraphql.t TEST 1 and
// TEST 14/15 at compatibilityOracleSourceCommit to executable differential
// cases. The compact valid query keeps the rewritten upstream body and URI
// deterministic for the harness's strict semantic comparison.
func differentialDegraphqlCases() []DifferentialCase {
	return []DifferentialCase{
		{
			Name:    "degraphql-post-query-without-variables",
			Plugin:  "degraphql",
			RouteID: "diff-degraphql-post-no-vars",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-degraphql-post-no-vars",
					"uri": "/graphql",
					"plugins": map[string]any{
						"degraphql": map[string]any{"query": differentialDegraphqlQuery},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodPost,
				Path:   "/graphql",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "graphql fixture response",
				},
			},
			SecurityDecision: "not_applicable",
		},
		{
			Name:    "degraphql-get-query-without-variables",
			Plugin:  "degraphql",
			RouteID: "diff-degraphql-get-no-vars",
			Config: map[string]any{
				"routes": []any{map[string]any{
					"id":  "diff-degraphql-get-no-vars",
					"uri": "/graphql",
					"plugins": map[string]any{
						"degraphql": map[string]any{"query": differentialDegraphqlQuery},
					},
					"upstream": differentialUpstream(),
				}},
			},
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/graphql",
				Host:   "gateway.example.test",
			},
			Fixture: DifferentialFixture{
				Name:          "primary",
				ExpectedCalls: 1,
				Response: DifferentialFixtureResponse{
					Status: http.StatusOK,
					Body:   "graphql fixture response",
				},
			},
			SecurityDecision: "not_applicable",
		},
	}
}
