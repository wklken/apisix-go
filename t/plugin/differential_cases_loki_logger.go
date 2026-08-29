package pluginintegration

import "net/http"

// differentialLokiLoggerCases maps APISIX 3.17 t/plugin/loki-logger.t
// TEST 2/3 and TEST 15/16 to one origin call plus one captured Loki push with
// a fixed label, tenant, authorization header, and custom log entry.
func differentialLokiLoggerCases() []DifferentialCase {
	const routeID = "differential-loki-logger-delivery"

	return []DifferentialCase{{
		Name:             "loki-logger-pushes-single-labelled-entry",
		Plugin:           "loki-logger",
		RouteID:          routeID,
		ComparisonPolicy: "loki-logger-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/loki",
				"plugins": map[string]any{
					"loki-logger": map[string]any{
						"endpoint_addrs": []any{"http://" + differentialFixturePlaceholder},
						"endpoint_uri":   "/loki/api/v1/push",
						"tenant_id":      "tenant-differential",
						"headers":        map[string]any{"Authorization": "test1234"},
						"log_labels":     map[string]any{"job": "apisix-differential"},
						"log_format":     map[string]any{"case": "loki-logger"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/loki", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-loki", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type", "X-Scope-OrgID"},
			Response:             DifferentialFixtureResponse{Status: http.StatusNoContent},
		},
	}}
}
