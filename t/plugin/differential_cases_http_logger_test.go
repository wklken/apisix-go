package pluginintegration

import "testing"

func TestDifferentialHTTPLoggerCasesCoverPinnedAPISIX317SingleDelivery(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialHTTPLoggerCases(), "http-logger")
	assertDifferentialLoggerSequence(t, spec, "http-logger-posts-single-formatted-entry", "/logger/http", 2)
	if spec.ComparisonPolicy != "http-logger-fixture-delivery" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	config := differentialLoggerPluginConfig(t, spec, "http-logger")
	if config["uri"] != "http://"+differentialFixturePlaceholder+"/http-log" ||
		config["auth_header"] != "Basic differential" ||
		config["log_format"].(map[string]any)["case"] != "http-logger" {
		t.Fatalf("http logger config = %#v", config)
	}
}
