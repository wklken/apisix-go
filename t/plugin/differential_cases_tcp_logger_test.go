package pluginintegration

import "testing"

func TestDifferentialTCPLoggerCasesCoverPinnedAPISIX317RouteFormatFrame(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	spec := onlyDifferentialLoggerCase(t, differentialTCPLoggerCases(), "tcp-logger")
	assertDifferentialLoggerSequence(
		t, spec, "tcp-logger-sends-single-route-format-frame", "/hello", 2,
	)
	if spec.ComparisonPolicy != differentialTCPLoggerFixtureDeliveryPolicy ||
		spec.Fixture.Name != "origin-and-tcp-log" || spec.Fixture.WireProtocol != "http-tcp" ||
		spec.Fixture.CollectTimeoutMillis != 6000 || len(spec.Fixture.SemanticHeaders) != 0 {
		t.Fatalf("policy/fixture = %q/%#v", spec.ComparisonPolicy, spec.Fixture)
	}
	if spec.Fixture.Response.Status != 200 || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}

	config := differentialLoggerPluginConfig(t, spec, "tcp-logger")
	if config["host"] != differentialFixtureHostPlaceholder ||
		config["port"] != differentialFixturePortPlaceholder || config["tls"] != false ||
		config["batch_max_size"] != 1 || config["max_retry_count"] != 0 ||
		config["buffer_duration"] != 1 || config["inactive_timeout"] != 1 {
		t.Fatalf("tcp logger delivery config = %#v", config)
	}
	format, ok := config["log_format"].(map[string]any)
	if !ok || len(format) != 4 || format["case name"] != "logger format in plugin" ||
		format["vip"] != "$remote_addr" || format["status"] != "$status" ||
		format["service_id"] != "stale-service" {
		t.Fatalf("tcp logger format = %#v", config["log_format"])
	}

	metadata := differentialTCPLoggerMetadata(t, spec)
	metadataFormat, ok := metadata["log_format"].(map[string]any)
	if !ok || len(metadataFormat) != 1 || metadataFormat["case name"] != "metadata should lose" {
		t.Fatalf("tcp logger metadata = %#v", metadata)
	}
}

func differentialTCPLoggerMetadata(t *testing.T, spec DifferentialCase) map[string]any {
	t.Helper()
	metadataList, ok := spec.Config["plugin_metadata"].([]any)
	if !ok || len(metadataList) != 1 {
		t.Fatalf("plugin metadata = %#v", spec.Config["plugin_metadata"])
	}
	metadata, ok := metadataList[0].(map[string]any)
	if !ok || metadata["id"] != "tcp-logger" {
		t.Fatalf("tcp logger metadata = %#v", metadataList[0])
	}
	return metadata
}
