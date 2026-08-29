package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialSyslogCasesCoverPinnedAPISIX317SingleTCPDelivery(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialSyslogCases()
	if len(cases) != 1 {
		t.Fatalf("syslog differential cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "syslog-sends-single-rfc5424-frame-over-tcp" || spec.Plugin != "syslog" ||
		spec.ComparisonPolicy != differentialSyslogTCPDeliveryPolicy {
		t.Fatalf("syslog case identity = %#v", spec)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != differentialSyslogGatewayPath {
		t.Fatalf("syslog steps = %#v", spec.Steps)
	}
	if spec.Fixture.WireProtocol != "http-tcp" || spec.Fixture.ExpectedCalls != 2 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "fixture-ok" {
		t.Fatalf("syslog fixture = %#v", spec.Fixture)
	}
	config := differentialSyslogPluginConfig(t, spec)
	if config["host"] != differentialFixtureHostPlaceholder ||
		config["port"] != differentialFixturePortPlaceholder || config["sock_type"] != "tcp" ||
		config["tls"] != false || config["flush_limit"] != 1 || config["batch_max_size"] != 1 {
		t.Fatalf("syslog config = %#v", config)
	}
	if !reflect.DeepEqual(config["log_format"], map[string]any{"case": "syslog"}) {
		t.Fatalf("syslog log_format = %#v", config["log_format"])
	}
	if plugins := differentialRequiredPluginNames(
		cases,
	); !reflect.DeepEqual(
		plugins,
		[]string{"prometheus", "syslog"},
	) {
		t.Fatalf("syslog required plugins = %v", plugins)
	}
}

func differentialSyslogPluginConfig(t *testing.T, spec DifferentialCase) map[string]any {
	t.Helper()
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("syslog routes = %#v", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("syslog route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("syslog plugins = %#v", route["plugins"])
	}
	config, ok := plugins["syslog"].(map[string]any)
	if !ok {
		t.Fatalf("syslog plugin config = %#v", plugins["syslog"])
	}
	return config
}
