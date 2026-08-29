package pluginintegration

import "testing"

func TestDifferentialSkyWalkingLoggerCasesCoverPinnedAPISIX317RouteLogFormat(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	spec := onlyDifferentialLoggerCase(t, differentialSkyWalkingLoggerCases(), "skywalking-logger")
	assertDifferentialLoggerSequence(
		t,
		spec,
		"skywalking-logger-posts-single-route-format-entry",
		"/logger/skywalking",
		2,
	)
	if spec.ComparisonPolicy != differentialSkyWalkingLoggerFixtureDeliveryPolicy ||
		spec.Fixture.Name != "origin-and-skywalking" ||
		spec.Fixture.CollectTimeoutMillis != 6000 {
		t.Fatalf("policy/fixture = %q/%#v", spec.ComparisonPolicy, spec.Fixture)
	}
	if len(spec.Fixture.SemanticHeaders) != 1 || spec.Fixture.SemanticHeaders[0] != "Content-Type" {
		t.Fatalf("semantic headers = %#v, want Content-Type only", spec.Fixture.SemanticHeaders)
	}

	config := differentialLoggerPluginConfig(t, spec, "skywalking-logger")
	if config["endpoint_addr"] != "http://"+differentialFixturePlaceholder ||
		config["batch_max_size"] != 1 || config["max_retry_count"] != 0 ||
		config["buffer_duration"] != 1 || config["inactive_timeout"] != 1 {
		t.Fatalf("skywalking delivery config = %#v", config)
	}
	format, ok := config["log_format"].(map[string]any)
	if !ok || len(format) != 1 || format["my_ip"] != "$remote_addr" {
		t.Fatalf("skywalking log format = %#v", config["log_format"])
	}
	if _, exists := config["service_name"]; exists {
		t.Fatalf("service_name overrides APISIX 3.17 default: %#v", config)
	}
	if _, exists := config["service_instance_name"]; exists {
		t.Fatalf("service_instance_name overrides APISIX 3.17 default: %#v", config)
	}
}
