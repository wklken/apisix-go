package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialUDPLoggerCasesCoverPinnedAPISIX317MetadataDatagram(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	spec := onlyDifferentialLoggerCase(t, differentialUDPLoggerCases(), "udp-logger")
	if spec.Name != "udp-logger-sends-single-metadata-format-datagram" || spec.RouteID == "" {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf("legacy request fields = %#v/%q, want sequence-only case", spec.Request, spec.SecurityDecision)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Request.Method != http.MethodGet ||
		spec.Steps[0].Request.Path != "/hello" || spec.Steps[0].Request.Host != "localhost" ||
		spec.Steps[0].SecurityDecision != "not_applicable" {
		t.Fatalf("steps = %#v", spec.Steps)
	}
	if spec.ComparisonPolicy != differentialUDPLoggerFixtureDeliveryPolicy ||
		spec.Fixture.Name != "origin-and-udp-log" || spec.Fixture.WireProtocol != "http-udp" ||
		spec.Fixture.CollectTimeoutMillis != 6000 || len(spec.Fixture.SemanticHeaders) != 0 {
		t.Fatalf("policy/fixture = %q/%#v", spec.ComparisonPolicy, spec.Fixture)
	}
	if spec.Fixture.Response.Status != 200 || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}
	if spec.Fixture.ExpectedCalls != 2 {
		t.Fatalf("fixture call count = %d, want origin plus one datagram", spec.Fixture.ExpectedCalls)
	}

	config := differentialLoggerPluginConfig(t, spec, "udp-logger")
	if config["host"] != differentialFixtureHostPlaceholder ||
		config["port"] != differentialFixturePortPlaceholder || config["batch_max_size"] != 1 ||
		config["max_retry_count"] != 0 || config["buffer_duration"] != 1 ||
		config["inactive_timeout"] != 1 {
		t.Fatalf("udp logger delivery config = %#v", config)
	}

	metadataList, ok := spec.Config["plugin_metadata"].([]any)
	if !ok || len(metadataList) != 1 {
		t.Fatalf("plugin metadata = %#v", spec.Config["plugin_metadata"])
	}
	metadata, ok := metadataList[0].(map[string]any)
	if !ok || metadata["id"] != "udp-logger" {
		t.Fatalf("udp logger metadata = %#v", metadataList[0])
	}
	format, ok := metadata["log_format"].(map[string]any)
	if !ok || len(format) != 4 || format["case name"] != "logger format in plugin" ||
		format["host"] != "$host" || format["client_ip"] != "$remote_addr" ||
		format["@timestamp"] != "$time_iso8601" {
		t.Fatalf("udp logger metadata format = %#v", metadata["log_format"])
	}
	if _, exists := config["log_format"]; exists {
		t.Fatalf("route config overrides metadata unexpectedly: %#v", config)
	}
}
