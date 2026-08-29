package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialBatchRequestsCasesCoverPinnedAPISIX317MissingPipeline(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialBatchRequestsCases()
	if len(cases) != 1 {
		t.Fatalf("differentialBatchRequestsCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "batch-requests-missing-pipeline" || spec.Plugin != "batch-requests" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-batch-requests-missing-pipeline" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.Request.Method != http.MethodPost || spec.Request.Path != "/apisix/batch-requests" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Request.Body != `{"pipeline1":[{"path":"/b"}]}` {
		t.Fatalf("request body = %q, want pinned TEST 3 missing-pipeline body", spec.Request.Body)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "" {
		t.Fatalf(
			"decision/policy = %q/%q, want not_applicable/exact",
			spec.SecurityDecision,
			spec.ComparisonPolicy,
		)
	}
	if got, want := differentialRequiredPluginNames(
		cases,
	), []string{
		"batch-requests",
		"public-api",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("required plugins = %v, want %v", got, want)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want pinned TEST 1 public-api dispatch route", spec.Config["routes"])
	}
	dispatch, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("dispatch route = %#v", routes[0])
	}
	if dispatch["id"] != spec.RouteID || dispatch["uri"] != "/apisix/batch-requests" {
		t.Fatalf("dispatch route identity = %#v", dispatch)
	}
	plugins, ok := dispatch["plugins"].(map[string]any)
	if !ok || len(plugins) != 1 {
		t.Fatalf("dispatch plugins = %#v, want only public-api", dispatch["plugins"])
	}
	publicAPI, ok := plugins["public-api"].(map[string]any)
	if !ok || len(publicAPI) != 0 {
		t.Fatalf("public-api config = %#v, want empty pinned config", plugins["public-api"])
	}
	if _, ok := dispatch["upstream"]; ok {
		t.Fatalf("dispatch upstream = %#v, want internal API dispatch without an upstream", dispatch["upstream"])
	}
}
