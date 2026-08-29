package pluginintegration

import "testing"

func TestDifferentialLokiLoggerCasesCoverPinnedAPISIX317PushAndHeaders(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialLokiLoggerCases(), "loki-logger")
	assertDifferentialLoggerSequence(t, spec, "loki-logger-pushes-single-labelled-entry", "/logger/loki", 2)
	if spec.ComparisonPolicy != "loki-logger-fixture-delivery" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	config := differentialLoggerPluginConfig(t, spec, "loki-logger")
	endpoints, ok := config["endpoint_addrs"].([]any)
	if !ok || len(endpoints) != 1 || endpoints[0] != "http://"+differentialFixturePlaceholder ||
		config["endpoint_uri"] != "/loki/api/v1/push" || config["tenant_id"] != "tenant-differential" {
		t.Fatalf("loki endpoint config = %#v", config)
	}
	if config["headers"].(map[string]any)["Authorization"] != "test1234" ||
		config["log_labels"].(map[string]any)["job"] != "apisix-differential" ||
		config["log_format"].(map[string]any)["case"] != "loki-logger" {
		t.Fatalf("loki semantic config = %#v", config)
	}
}
