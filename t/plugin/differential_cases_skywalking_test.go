package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialSkyWalkingCasesCoverPinnedAPISIX317FullSamplingSW8Boundary(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialSkyWalkingCases()
	if len(cases) != 1 {
		t.Fatalf("differentialSkyWalkingCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "skywalking-full-sampling-injects-valid-sw8" ||
		spec.Plugin != "skywalking" || spec.RouteID != "differential-skywalking-full-sampling" ||
		spec.ComparisonPolicy != differentialSkyWalkingSW8FullSamplingPolicy {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.Steps))
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet || step.Request.Path != "/opentracing" ||
		step.Request.Host != "gateway.example.test" || step.SecurityDecision != "not_applicable" {
		t.Fatalf("step = %#v", step)
	}

	plugin := differentialSkyWalkingPluginConfig(t, spec.Config)
	if len(plugin) != 1 || plugin["sample_ratio"] != 1 {
		t.Fatalf("skywalking config = %#v, want only full sampling", plugin)
	}
	if _, exists := spec.Config["plugin_attr"]; exists {
		t.Fatalf("case must not project an unobservable collector endpoint: %#v", spec.Config)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		!spec.Fixture.CaptureAllCalls || spec.Fixture.CollectTimeoutMillis != 5000 ||
		spec.Fixture.RequestWindowQuietMillis != 500 ||
		!reflect.DeepEqual(spec.Fixture.SemanticHeaders, []string{"sw8"}) ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "opentracing" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
}

func differentialSkyWalkingPluginConfig(t *testing.T, config map[string]any) map[string]any {
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
	plugin, ok := plugins["skywalking"].(map[string]any)
	if !ok {
		t.Fatalf("skywalking = %#v", plugins["skywalking"])
	}
	return plugin
}
