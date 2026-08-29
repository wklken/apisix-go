package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialSLSLoggerCasesCoverPinnedAPISIX317SingleTLSDelivery(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	cases := differentialSLSLoggerCases()
	if len(cases) != 1 {
		t.Fatalf("sls-logger differential cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "sls-logger-sends-single-rfc5424-frame-over-tls" || spec.Plugin != "sls-logger" ||
		spec.ComparisonPolicy != differentialSLSLoggerTLSDeliveryPolicy {
		t.Fatalf("sls-logger case identity = %#v", spec)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != differentialSLSLoggerGatewayPath {
		t.Fatalf("sls-logger steps = %#v", spec.Steps)
	}
	if spec.Fixture.WireProtocol != "tls-tcp" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "fixture-ok" {
		t.Fatalf("sls-logger fixture = %#v", spec.Fixture)
	}
	loggerConfig, mockingConfig := differentialSLSLoggerPluginConfigs(t, spec)
	if loggerConfig["host"] != differentialFixtureHostPlaceholder ||
		loggerConfig["port"] != differentialFixturePortPlaceholder ||
		loggerConfig["project"] != differentialSLSLoggerProject ||
		loggerConfig["logstore"] != differentialSLSLoggerLogstore ||
		loggerConfig["access_key_id"] != differentialSLSLoggerAccessKeyID ||
		loggerConfig["access_key_secret"] != differentialSLSLoggerAccessKeySecret ||
		loggerConfig["batch_max_size"] != 1 {
		t.Fatalf("sls-logger config = %#v", loggerConfig)
	}
	if _, exists := loggerConfig["ssl_verify"]; exists {
		t.Fatalf("sls-logger config adds Go-only ssl_verify: %#v", loggerConfig)
	}
	if !reflect.DeepEqual(loggerConfig["log_format"], map[string]any{"case": "sls-logger"}) {
		t.Fatalf("sls-logger log_format = %#v", loggerConfig["log_format"])
	}
	if mockingConfig["response_status"] != http.StatusOK || mockingConfig["response_example"] != "fixture-ok" ||
		mockingConfig["with_mock_header"] != false {
		t.Fatalf("sls-logger mocking config = %#v", mockingConfig)
	}
	if plugins := differentialRequiredPluginNames(cases); !reflect.DeepEqual(
		plugins, []string{"mocking", "prometheus", "sls-logger"},
	) {
		t.Fatalf("sls-logger required plugins = %v", plugins)
	}
}

func differentialSLSLoggerPluginConfigs(
	t *testing.T,
	spec DifferentialCase,
) (map[string]any, map[string]any) {
	t.Helper()
	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("sls-logger routes = %#v", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok {
		t.Fatalf("sls-logger route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("sls-logger plugins = %#v", route["plugins"])
	}
	loggerConfig, ok := plugins["sls-logger"].(map[string]any)
	if !ok {
		t.Fatalf("sls-logger plugin config = %#v", plugins["sls-logger"])
	}
	mockingConfig, ok := plugins["mocking"].(map[string]any)
	if !ok {
		t.Fatalf("mocking plugin config = %#v", plugins["mocking"])
	}
	return loggerConfig, mockingConfig
}
