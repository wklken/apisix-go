package pluginintegration

import "net/http"

// differentialSplunkHECLoggingCases maps APISIX 3.17
// t/plugin/splunk-hec-logging.t TEST 4/5 and TEST 9/10 to one origin request
// plus one captured, authenticated HEC event POST.
func differentialSplunkHECLoggingCases() []DifferentialCase {
	const routeID = "differential-splunk-hec-logging-delivery"

	return []DifferentialCase{{
		Name:             "splunk-hec-logging-posts-single-custom-event",
		Plugin:           "splunk-hec-logging",
		RouteID:          routeID,
		ComparisonPolicy: "splunk-hec-logging-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/splunk",
				"plugins": map[string]any{
					"splunk-hec-logging": map[string]any{
						"endpoint": map[string]any{
							"uri":   "http://" + differentialFixturePlaceholder + "/services/collector",
							"token": "BD274822-96AA-4DA6-90EC-18940FB2414C",
						},
						"log_format":     map[string]any{"message": "differential-splunk-event"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/splunk", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-splunk", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response: DifferentialFixtureResponse{
				Status:  http.StatusOK,
				Headers: map[string]string{"Content-Type": "application/json"},
				Body:    `{"text":"Success","code":0}`,
			},
		},
	}}
}
