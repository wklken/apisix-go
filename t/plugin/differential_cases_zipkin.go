package pluginintegration

import "net/http"

const (
	differentialZipkinTraceID        = "80f198ee56343ba864fe8b2a57d3eff7"
	differentialZipkinIncomingSpanID = "e457b5a2e4d86bd1"
	differentialZipkinIncomingB3     = differentialZipkinTraceID + "-" +
		differentialZipkinIncomingSpanID + "-d-05e3ac9a4f6e3b90"
	differentialZipkinCollectorPath = "/api/v2/spans"
)

// differentialZipkinCases maps the APISIX 3.17 zipkin.t and zipkin2.t v2
// request-span, single-header B3, debug-sampling, and HTTP reporter contracts
// to one bounded SERVER-span differential case. APISIX phase-span topology is
// validated by the comparator only to preserve the declared platform gap.
func differentialZipkinCases() []DifferentialCase {
	const routeID = "differential-zipkin-v2-server-span"

	return []DifferentialCase{{
		Name:             "zipkin-v2-exports-incoming-debug-child-server-span",
		Plugin:           "zipkin",
		RouteID:          routeID,
		ComparisonPolicy: "zipkin-v2-server-span-core",
		Config: map[string]any{
			"routes": []any{map[string]any{
				"id": routeID, "uri": "/zipkin/v2",
				"plugins": map[string]any{
					"zipkin": map[string]any{
						"endpoint":     "http://" + differentialFixturePlaceholder + differentialZipkinCollectorPath,
						"sample_ratio": 1, "service_name": "APISIX",
						"server_addr": "127.0.0.1", "span_version": 2,
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet, Path: "/zipkin/v2", Host: "gateway.example.test",
				Headers: map[string]string{"b3": differentialZipkinIncomingB3},
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name: "origin-and-zipkin-v2", ExpectedCalls: 2, CaptureAllCalls: true,
			CollectTimeoutMillis: 10000,
			SemanticHeaders: []string{
				"Content-Type", "X-B3-Flags", "X-B3-ParentSpanId", "X-B3-Sampled", "X-B3-SpanId", "X-B3-TraceId",
			},
			Response: DifferentialFixtureResponse{Status: http.StatusOK, Body: "fixture-ok"},
		},
	}}
}
