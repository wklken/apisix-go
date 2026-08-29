package pluginintegration

import "net/http"

// differentialLagoCases maps APISIX 3.17 t/plugin/lago.t TEST 2 to one
// proxied request and one authenticated, single-event batch delivery.
func differentialLagoCases() []DifferentialCase {
	const routeID = "differential-lago-delivery"

	return []DifferentialCase{{
		Name:             "lago-posts-single-usage-event",
		Plugin:           "lago",
		RouteID:          routeID,
		ComparisonPolicy: "lago-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/lago",
				"plugins": map[string]any{
					"lago": map[string]any{
						"endpoint_addrs":        []any{"http://" + differentialFixturePlaceholder},
						"token":                 "differential-token",
						"event_transaction_id":  "${http_x_request_id}",
						"event_subscription_id": "differential-subscription",
						"event_code":            "differential-usage",
						"event_properties": map[string]any{
							"route": "${route_id}", "status": "${status}",
						},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/lago", Host: "gateway.example.test",
				Headers: map[string]string{"X-Request-Id": "differential-request"},
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-lago", ExpectedCalls: 2, CollectTimeoutMillis: 6000,
			SemanticHeaders: []string{"Authorization", "Content-Type"},
			Response:        DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
