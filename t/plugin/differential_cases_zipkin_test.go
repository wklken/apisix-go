package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialZipkinCasesCoverPinnedAPISIX317V2ServerSpanCore(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialZipkinCases()
	if len(cases) != 1 {
		t.Fatalf("differentialZipkinCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "zipkin-v2-exports-incoming-debug-child-server-span" ||
		spec.Plugin != "zipkin" || spec.RouteID != "differential-zipkin-v2-server-span" ||
		spec.ComparisonPolicy != "zipkin-v2-server-span-core" {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet || step.Request.Path != "/zipkin/v2" ||
		step.Request.Host != "gateway.example.test" ||
		step.Request.Headers["b3"] != differentialZipkinIncomingB3 ||
		step.SecurityDecision != "not_applicable" {
		t.Fatalf("step = %#v", step)
	}

	config := differentialZipkinPluginConfig(t, spec.Config)
	if config["endpoint"] != "http://"+differentialFixturePlaceholder+"/api/v2/spans" ||
		config["sample_ratio"] != 1 || config["service_name"] != "APISIX" ||
		config["server_addr"] != "127.0.0.1" || config["span_version"] != 2 {
		t.Fatalf("zipkin config = %#v", config)
	}
	projected, err := projectDifferentialConfig(spec.Config, "127.0.0.1:31111")
	if err != nil {
		t.Fatalf("project numeric loopback fixture: %v", err)
	}
	projectedConfig := differentialZipkinPluginConfig(t, projected)
	if projectedConfig["endpoint"] != "http://127.0.0.1:31111/api/v2/spans" {
		t.Fatalf("projected endpoint = %#v", projectedConfig["endpoint"])
	}

	wantHeaders := []string{
		"Content-Type", "X-B3-Flags", "X-B3-ParentSpanId", "X-B3-Sampled", "X-B3-SpanId", "X-B3-TraceId",
	}
	if spec.Fixture.Name != "origin-and-zipkin-v2" || spec.Fixture.ExpectedCalls != 2 ||
		!spec.Fixture.CaptureAllCalls || spec.Fixture.CollectTimeoutMillis != 10000 ||
		!reflect.DeepEqual(spec.Fixture.SemanticHeaders, wantHeaders) ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "fixture-ok" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
}

func differentialZipkinPluginConfig(t *testing.T, config map[string]any) map[string]any {
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
	zipkin, ok := plugins["zipkin"].(map[string]any)
	if !ok {
		t.Fatalf("zipkin config = %#v", plugins["zipkin"])
	}
	return zipkin
}
