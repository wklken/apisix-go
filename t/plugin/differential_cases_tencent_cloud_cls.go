package pluginintegration

import "net/http"

// differentialTencentCloudCLSCases maps APISIX 3.17
// t/plugin/tencent-cloud-cls.t TEST 5/6 and TEST 13/14 to one origin request
// plus one captured, signed protobuf CLS request. This local fixture proves the
// HTTP/protobuf protocol shape only; it does not claim a live Tencent service.
func differentialTencentCloudCLSCases() []DifferentialCase {
	const routeID = "differential-tencent-cloud-cls-delivery"

	return []DifferentialCase{{
		Name:             "tencent-cloud-cls-posts-single-formatted-log",
		Plugin:           "tencent-cloud-cls",
		RouteID:          routeID,
		ComparisonPolicy: "tencent-cloud-cls-fixture-delivery",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/logger/tencent-cls",
				"plugins": map[string]any{
					"tencent-cloud-cls": map[string]any{
						"scheme": "http", "cls_host": differentialFixturePlaceholder,
						"cls_topic": "143b5d70-139b-4aec-b54e-bb97756916de",
						"secret_id": "secret_id", "secret_key": "secret_key",
						"log_format":     map[string]any{"case": "tencent-cloud-cls"},
						"batch_max_size": 1, "max_retry_count": 0,
						"buffer_duration": 1, "inactive_timeout": 1,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/logger/tencent-cls", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-tencent-cls", ExpectedCalls: 2,
			CollectTimeoutMillis: 6000,
			SemanticHeaders:      []string{"Content-Type"},
			Response:             DifferentialFixtureResponse{Status: http.StatusOK, Body: "ok"},
		},
	}}
}
