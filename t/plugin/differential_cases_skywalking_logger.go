package pluginintegration

import "net/http"

const (
	differentialSkyWalkingLoggerFixtureDeliveryPolicy = "skywalking-logger-fixture-delivery"
	differentialSkyWalkingLoggerPath                  = "/v3/logs"
)

// differentialSkyWalkingLoggerCases maps APISIX 3.17
// t/plugin/skywalking-logger.t TEST 10/11 to one origin GET plus one captured
// SkyWalking /v3/logs POST. The route-local format deliberately contains only
// my_ip; APISIX adds route_id to that custom payload.
func differentialSkyWalkingLoggerCases() []DifferentialCase {
	const routeID = "differential-skywalking-logger-delivery"

	return []DifferentialCase{{
		Name:             "skywalking-logger-posts-single-route-format-entry",
		Plugin:           "skywalking-logger",
		RouteID:          routeID,
		ComparisonPolicy: differentialSkyWalkingLoggerFixtureDeliveryPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/skywalking",
				"plugins": map[string]any{
					"skywalking-logger": map[string]any{
						"endpoint_addr":  "http://" + differentialFixturePlaceholder,
						"log_format":     map[string]any{"my_ip": "$remote_addr"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/skywalking", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-skywalking", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response:             DifferentialFixtureResponse{Status: http.StatusOK},
		},
	}}
}
