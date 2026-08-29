package pluginintegration

import "net/http"

const (
	differentialOpenTelemetryOTLPHTTPServerSpanPolicy = "opentelemetry-otlp-http-server-span-core"
	differentialOpenTelemetryRouteID                  = "differential-opentelemetry-otlp-http"
	differentialOpenTelemetryRouteName                = "differential-opentelemetry-route"
	differentialOpenTelemetryServiceName              = "differential-apisix"
	differentialOpenTelemetryTraceID                  = "01010101010101010101010101010101"
	differentialOpenTelemetryCollectorPath            = "/v1/traces"
)

// differentialOpenTelemetryCases maps APISIX 3.17 opentelemetry.t TEST 1-4:
// metadata admits inactive_timeout=0.5 and configures OTLP/HTTP, the route uses
// always-on sampling, a request creates a server span, and the collector
// receives the protobuf export. max_export_batch_size=1 avoids claiming that
// the Lua inactive timer has an exact Go SDK equivalent.
func differentialOpenTelemetryCases() []DifferentialCase {
	return []DifferentialCase{{
		Name:             "opentelemetry-exports-pinned-otlp-http-server-span",
		Plugin:           "opentelemetry",
		RouteID:          differentialOpenTelemetryRouteID,
		ComparisonPolicy: differentialOpenTelemetryOTLPHTTPServerSpanPolicy,
		Config: map[string]any{
			"plugin_metadata": []any{map[string]any{
				"id":              "opentelemetry",
				"trace_id_source": "x-request-id",
				"resource": map[string]any{
					"service.name": differentialOpenTelemetryServiceName,
				},
				"collector": map[string]any{
					"address":         "http://" + differentialFixturePlaceholder,
					"request_timeout": 3,
					"request_headers": map[string]any{
						"X-Differential-OTel": "contract-v1",
					},
				},
				"batch_span_processor": map[string]any{
					"max_export_batch_size": 1,
					"inactive_timeout":      0.5,
				},
			}},
			"routes": []any{map[string]any{
				"id":   differentialOpenTelemetryRouteID,
				"name": differentialOpenTelemetryRouteName,
				"uri":  "/otel/trace",
				"plugins": map[string]any{
					"opentelemetry": map[string]any{
						"sampler":                             map[string]any{"name": "always_on"},
						"additional_attributes":               []any{"arg_tenant"},
						"additional_header_prefix_attributes": []any{"x-tenant"},
					},
				},
				"upstream": differentialUpstream(),
			}},
		},
		Steps: []DifferentialStep{{
			Request: DifferentialRequest{
				Method: http.MethodGet,
				Path:   "/otel/trace?tenant=blue",
				Host:   "gateway.example.test",
				Headers: map[string]string{
					"X-Request-Id": differentialOpenTelemetryTraceID,
					"X-Tenant":     "blue",
				},
			},
			SecurityDecision: "not_applicable",
		}},
		Fixture: DifferentialFixture{
			Name:                 "origin-and-opentelemetry-otlp-http",
			ExpectedCalls:        2,
			CaptureAllCalls:      true,
			CollectTimeoutMillis: 7000,
			SemanticHeaders: []string{
				"Content-Encoding", "Content-Type", "X-Differential-OTel", "X-Request-Id", "X-Tenant",
			},
			Response: DifferentialFixtureResponse{Status: http.StatusOK},
		},
	}}
}
