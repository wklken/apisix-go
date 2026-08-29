package pluginintegration

import (
	"net/http"
	"slices"
	"testing"
)

func TestDifferentialPrometheusCasesCoverPinnedAPISIX317RouteStatusSeries(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialPrometheusCases()
	if len(cases) != 1 {
		t.Fatalf("differentialPrometheusCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "prometheus-records-route-status-series" || spec.Plugin != "prometheus" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-prometheus-route" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.ComparisonPolicy != "prometheus-route-status-series" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 2 {
		t.Fatalf("steps = %d, want request then scrape", len(spec.Steps))
	}
	if got := spec.Steps[0]; got.Request.Method != http.MethodGet || got.Request.Path != "/profile" ||
		got.Request.Host != "gateway.example.test" || got.SecurityDecision != "not_applicable" {
		t.Fatalf("record step = %#v", got)
	}
	if got := spec.Steps[1]; got.Request.Method != http.MethodGet ||
		got.Request.Path != "/apisix/prometheus/metrics" ||
		got.Request.Host != "gateway.example.test" || got.SecurityDecision != "not_applicable" ||
		got.DelayBeforeMillis != 1500 {
		t.Fatalf("scrape step = %#v", got)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "profile-ok" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if got := differentialRequiredPluginNames(cases); !slices.Equal(got, []string{"prometheus", "public-api"}) {
		t.Fatalf("required plugins = %v, want prometheus and public-api", got)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 2 {
		t.Fatalf("routes = %#v, want metrics and profile routes", spec.Config["routes"])
	}
	metricsRoute, ok := routes[0].(map[string]any)
	if !ok || metricsRoute["uri"] != "/apisix/prometheus/metrics" {
		t.Fatalf("metrics route = %#v", routes[0])
	}
	metricsPlugins, ok := metricsRoute["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("metrics plugins = %#v", metricsRoute["plugins"])
	}
	if _, ok := metricsPlugins["public-api"].(map[string]any); !ok {
		t.Fatalf("public-api config = %#v", metricsPlugins["public-api"])
	}

	profileRoute, ok := routes[1].(map[string]any)
	if !ok || profileRoute["id"] != spec.RouteID || profileRoute["uri"] != "/profile" {
		t.Fatalf("profile route = %#v", routes[1])
	}
	profilePlugins, ok := profileRoute["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("profile plugins = %#v", profileRoute["plugins"])
	}
	if _, ok := profilePlugins["prometheus"].(map[string]any); !ok {
		t.Fatalf("prometheus config = %#v", profilePlugins["prometheus"])
	}
	upstream, ok := profileRoute["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("upstream = %#v", profileRoute["upstream"])
	}
	nodes, ok := upstream["nodes"].(map[string]any)
	if !ok || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream nodes = %#v", upstream["nodes"])
	}
}
