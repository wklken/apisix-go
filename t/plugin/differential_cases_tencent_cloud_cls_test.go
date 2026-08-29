package pluginintegration

import "testing"

func TestDifferentialTencentCloudCLSCasesCoverPinnedAPISIX317SignedProtobuf(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialTencentCloudCLSCases(), "tencent-cloud-cls")
	assertDifferentialLoggerSequence(t, spec, "tencent-cloud-cls-posts-single-formatted-log", "/logger/tencent-cls", 2)
	if spec.ComparisonPolicy != "tencent-cloud-cls-fixture-delivery" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	config := differentialLoggerPluginConfig(t, spec, "tencent-cloud-cls")
	if config["scheme"] != "http" || config["cls_host"] != differentialFixturePlaceholder ||
		config["cls_topic"] != "143b5d70-139b-4aec-b54e-bb97756916de" ||
		config["secret_id"] != "secret_id" || config["secret_key"] != "secret_key" ||
		config["log_format"].(map[string]any)["case"] != "tencent-cloud-cls" {
		t.Fatalf("Tencent CLS config = %#v", config)
	}
}
