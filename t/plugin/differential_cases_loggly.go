package pluginintegration

import "net/http"

const differentialLogglyBulkPath = "/loggly/bulk/differential-token/tag/bulk"

// differentialLogglyCases maps APISIX 3.17 t/plugin/loggly.t TEST 15 to one
// proxied request and one captured HTTP bulk delivery.
func differentialLogglyCases() []DifferentialCase {
	const routeID = "differential-loggly-http-delivery"

	return []DifferentialCase{{
		Name:             "loggly-http-posts-single-formatted-entry",
		Plugin:           "loggly",
		RouteID:          routeID,
		ComparisonPolicy: "loggly-http-fixture-delivery",
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id":       "loggly",
				"host":     differentialFixturePlaceholder + "/loggly",
				"protocol": "http",
				"log_format": map[string]any{
					"case": "loggly", "route_id": "$route_id", "timestamp": "$time_iso8601",
				},
			}},
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/loggly",
				"plugins": map[string]any{
					"loggly": map[string]any{
						"customer_token": "differential-token", "batch_max_size": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/loggly", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-loggly", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type", "X-LOGGLY-TAG"},
			Response: DifferentialFixtureResponse{
				Status: http.StatusOK, Body: "fixture-ok",
			},
		},
	}}
}
