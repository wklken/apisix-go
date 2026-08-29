package pluginintegration

import "testing"

func TestDifferentialSplunkHECLoggingCasesCoverPinnedAPISIX317CustomEvent(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialSplunkHECLoggingCases(), "splunk-hec-logging")
	assertDifferentialLoggerSequence(t, spec, "splunk-hec-logging-posts-single-custom-event", "/logger/splunk", 2)
	if spec.ComparisonPolicy != "splunk-hec-logging-fixture-delivery" ||
		spec.Fixture.Response.Body != `{"text":"Success","code":0}` {
		t.Fatalf("policy/fixture = %q/%#v", spec.ComparisonPolicy, spec.Fixture.Response)
	}
	config := differentialLoggerPluginConfig(t, spec, "splunk-hec-logging")
	endpoint := config["endpoint"].(map[string]any)
	if endpoint["uri"] != "http://"+differentialFixturePlaceholder+"/services/collector" ||
		endpoint["token"] != "BD274822-96AA-4DA6-90EC-18940FB2414C" ||
		config["log_format"].(map[string]any)["message"] != "differential-splunk-event" {
		t.Fatalf("splunk config = %#v", config)
	}
}
