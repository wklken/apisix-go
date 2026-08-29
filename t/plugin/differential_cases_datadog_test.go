package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialDatadogCasesPreserveSixAPISIX317DogStatsDDatagrams(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialDatadogCases()
	if len(cases) != 1 {
		t.Fatalf("datadog cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "datadog-emits-six-ordered-single-metric-datagrams" ||
		spec.Plugin != "datadog" || spec.RouteID != differentialDatadogRouteID ||
		spec.ComparisonPolicy != differentialDatadogSixDatagramsPolicy {
		t.Fatalf("case identity = %#v", spec)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != differentialDatadogGatewayPath ||
		spec.Steps[0].Request.Host != "gateway.example.test" ||
		spec.Steps[0].SecurityDecision != "not_applicable" {
		t.Fatalf("gateway step = %#v", spec.Steps)
	}
	if spec.Fixture.Name != "datadog-http-origin-and-udp-sink" ||
		spec.Fixture.WireProtocol != differentialDatadogHTTPUDPWireProtocol ||
		spec.Fixture.ExpectedCalls != 6 || !spec.Fixture.CaptureAllCalls ||
		!spec.Fixture.OmitHTTPOriginCall ||
		spec.Fixture.CollectTimeoutMillis != 5000 ||
		spec.Fixture.Response.Status != http.StatusOK ||
		spec.Fixture.Response.Headers["Content-Type"] != "text/plain" ||
		spec.Fixture.Response.Body != "opentracing" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}

	metadata := spec.Config["plugin_metadata"].([]any)
	if len(metadata) != 1 {
		t.Fatalf("plugin metadata = %#v", metadata)
	}
	endpoint := metadata[0].(map[string]any)
	if endpoint["id"] != "datadog" || endpoint["host"] != differentialFixtureHostPlaceholder ||
		endpoint["port"] != differentialFixturePortPlaceholder {
		t.Fatalf("Datadog endpoint = %#v", endpoint)
	}
	routes := spec.Config["routes"].([]any)
	if len(routes) != 1 {
		t.Fatalf("routes = %#v", routes)
	}
	route := routes[0].(map[string]any)
	if route["id"] != differentialDatadogRouteID || route["name"] != "datadog" ||
		route["uri"] != differentialDatadogGatewayPath {
		t.Fatalf("route identity = %#v", route)
	}
	config := route["plugins"].(map[string]any)["datadog"].(map[string]any)
	if config["batch_max_size"] != 1 || config["max_retry_count"] != 0 ||
		config["retry_delay"] != 0 {
		t.Fatalf("Datadog config = %#v", config)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", upstream)
	}
}
