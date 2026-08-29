package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialClickHouseLoggerCasesCoverPinnedAPISIX317SingleDelivery(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	spec := onlyDifferentialLoggerCase(t, differentialClickHouseLoggerCases(), "clickhouse-logger")
	assertDifferentialLoggerSequence(t, spec, "clickhouse-logger-posts-single-formatted-entry", "/logger/clickhouse", 2)
	config := differentialLoggerPluginConfig(t, spec, "clickhouse-logger")
	if config["endpoint_addr"] != "http://"+differentialFixturePlaceholder+"/clickhouse" ||
		config["user"] != "default" || config["password"] != "differential-password" ||
		config["database"] != "default" || config["logtable"] != "logs" {
		t.Fatalf("clickhouse config = %#v", config)
	}
	if got := config["log_format"].(map[string]any)["case"]; got != "clickhouse-logger" {
		t.Fatalf("log format case = %#v", got)
	}
}

func onlyDifferentialLoggerCase(t *testing.T, cases []DifferentialCase, plugin string) DifferentialCase {
	t.Helper()
	if len(cases) != 1 || cases[0].Plugin != plugin {
		t.Fatalf("%s cases = %#v, want one case", plugin, cases)
	}
	return cases[0]
}

func assertDifferentialLoggerSequence(
	t *testing.T, spec DifferentialCase, name, path string, expectedCalls int,
) {
	t.Helper()
	if spec.Name != name || spec.RouteID == "" || spec.ComparisonPolicy == "" {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != path || spec.Steps[0].Request.Host != "gateway.example.test" ||
		spec.Steps[0].SecurityDecision != "not_applicable" {
		t.Fatalf("steps = %#v", spec.Steps)
	}
	if spec.Fixture.ExpectedCalls != expectedCalls || spec.Fixture.Response.Status < 200 ||
		spec.Fixture.Response.Status >= 300 {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
}

func differentialLoggerPluginConfig(t *testing.T, spec DifferentialCase, plugin string) map[string]any {
	t.Helper()
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	config, ok := plugins[plugin].(map[string]any)
	if !ok {
		t.Fatalf("%s config = %#v", plugin, plugins[plugin])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok || upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", route["upstream"])
	}
	return config
}
