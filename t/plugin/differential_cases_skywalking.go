package pluginintegration

import "net/http"

const differentialSkyWalkingSW8FullSamplingPolicy = "skywalking-sw8-full-sampling"

// differentialSkyWalkingCases maps the route admission and request trigger in
// APISIX 3.17 t/plugin/skywalking.t TEST 1/2 to one observable boundary: with
// full sampling enabled, the HTTP origin receives a structurally valid SW8
// header. Collector reporting is deliberately outside this fixture because
// the differential harness cannot project plugin_attr.skywalking.endpoint_addr.
func differentialSkyWalkingCases() []DifferentialCase {
	const routeID = "differential-skywalking-full-sampling"

	return []DifferentialCase{{
		Name:             "skywalking-full-sampling-injects-valid-sw8",
		Plugin:           "skywalking",
		RouteID:          routeID,
		ComparisonPolicy: differentialSkyWalkingSW8FullSamplingPolicy,
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/opentracing",
				"plugins": map[string]any{
					"skywalking": map[string]any{"sample_ratio": 1},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/opentracing", Host: "gateway.example.test",
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "primary", ExpectedCalls: 1, CaptureAllCalls: true,
			CollectTimeoutMillis: 5000, RequestWindowQuietMillis: 500,
			SemanticHeaders: []string{"sw8"},
			Response:        DifferentialFixtureResponse{Status: http.StatusOK, Body: "opentracing"},
		},
	}}
}
