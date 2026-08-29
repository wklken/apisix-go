package pluginintegration

import "testing"

func TestDifferentialElasticsearchLoggerCasesCoverPinnedAPISIX317CustomBulk(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialElasticsearchLoggerCases(), "elasticsearch-logger")
	assertDifferentialLoggerSequence(
		t,
		spec,
		"elasticsearch-logger-posts-single-custom-entry",
		"/logger/elasticsearch",
		3,
	)
	if spec.ComparisonPolicy != "elasticsearch-logger-fixture-delivery" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	if spec.Fixture.Response.Body != `{"version":{"number":"8.10.2"}}` {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}
	config := differentialLoggerPluginConfig(t, spec, "elasticsearch-logger")
	if config["endpoint_addr"] != "http://"+differentialFixturePlaceholder ||
		config["field"].(map[string]any)["index"] != "services" ||
		config["log_format"].(map[string]any)["custom_case"] != "elasticsearch-logger" {
		t.Fatalf("elasticsearch config = %#v", config)
	}
}
