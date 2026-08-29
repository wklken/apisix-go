package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialOpenTelemetryCasesCoverPinnedAPISIX317OTLPHTTPServerSpan(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialOpenTelemetryCases()
	if len(cases) != 1 {
		t.Fatalf("differentialOpenTelemetryCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "opentelemetry-exports-pinned-otlp-http-server-span" ||
		spec.Plugin != "opentelemetry" || spec.RouteID != differentialOpenTelemetryRouteID ||
		spec.ComparisonPolicy != differentialOpenTelemetryOTLPHTTPServerSpanPolicy {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet || step.Request.Path != "/otel/trace?tenant=blue" ||
		step.Request.Host != "gateway.example.test" ||
		step.Request.Headers["X-Request-Id"] != differentialOpenTelemetryTraceID ||
		step.Request.Headers["X-Tenant"] != "blue" ||
		step.SecurityDecision != "not_applicable" {
		t.Fatalf("step = %#v", step)
	}

	metadata := differentialOpenTelemetryMetadata(t, spec.Config)
	collector := metadata["collector"].(map[string]any)
	batch := metadata["batch_span_processor"].(map[string]any)
	resource := metadata["resource"].(map[string]any)
	if metadata["trace_id_source"] != "x-request-id" ||
		collector["address"] != "http://"+differentialFixturePlaceholder ||
		collector["request_timeout"] != 3 ||
		collector["request_headers"].(map[string]any)["X-Differential-OTel"] != "contract-v1" ||
		batch["max_export_batch_size"] != 1 || batch["inactive_timeout"] != 0.5 ||
		resource["service.name"] != differentialOpenTelemetryServiceName {
		t.Fatalf("plugin metadata = %#v", metadata)
	}
	pluginConfig := differentialOpenTelemetryPluginConfig(t, spec.Config)
	if pluginConfig["sampler"].(map[string]any)["name"] != "always_on" ||
		!reflect.DeepEqual(pluginConfig["additional_attributes"], []any{"arg_tenant"}) ||
		!reflect.DeepEqual(pluginConfig["additional_header_prefix_attributes"], []any{"x-tenant"}) {
		t.Fatalf("opentelemetry route config = %#v", pluginConfig)
	}
	projected, err := projectDifferentialConfig(spec.Config, "127.0.0.1:31111")
	if err != nil {
		t.Fatalf("project numeric loopback fixture: %v", err)
	}
	if got := differentialOpenTelemetryMetadata(t, projected)["collector"].(map[string]any)["address"]; got != "http://127.0.0.1:31111" {
		t.Fatalf("projected collector address = %#v", got)
	}

	wantHeaders := []string{
		"Content-Encoding", "Content-Type", "X-Differential-OTel", "X-Request-Id", "X-Tenant",
	}
	if spec.Fixture.Name != "origin-and-opentelemetry-otlp-http" ||
		spec.Fixture.ExpectedCalls != 2 || !spec.Fixture.CaptureAllCalls ||
		spec.Fixture.CollectTimeoutMillis != 7000 ||
		!reflect.DeepEqual(spec.Fixture.SemanticHeaders, wantHeaders) ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
}

func differentialOpenTelemetryMetadata(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	raw, ok := config["plugin_metadata"].([]any)
	if !ok || len(raw) != 1 {
		t.Fatalf("plugin metadata = %#v", config["plugin_metadata"])
	}
	metadata, ok := raw[0].(map[string]any)
	if !ok || metadata["id"] != "opentelemetry" {
		t.Fatalf("opentelemetry metadata = %#v", raw[0])
	}
	return metadata
}

func differentialOpenTelemetryPluginConfig(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	routes, ok := config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	pluginConfig, ok := plugins["opentelemetry"].(map[string]any)
	if !ok {
		t.Fatalf("opentelemetry config = %#v", plugins["opentelemetry"])
	}
	return pluginConfig
}
