package pluginintegration

import "testing"

func TestDifferentialLagoCasesCoverPinnedAPISIX317UsageEventDelivery(t *testing.T) {
	spec := onlyDifferentialLoggerCase(t, differentialLagoCases(), "lago")
	assertDifferentialLoggerSequence(t, spec, "lago-posts-single-usage-event", "/logger/lago", 2)
	if spec.ComparisonPolicy != "lago-fixture-delivery" {
		t.Fatalf("comparison policy = %q", spec.ComparisonPolicy)
	}
	if spec.Steps[0].Request.Headers["X-Request-Id"] != "differential-request" {
		t.Fatalf("request headers = %#v", spec.Steps[0].Request.Headers)
	}
	config := differentialLoggerPluginConfig(t, spec, "lago")
	endpointAddrs, ok := config["endpoint_addrs"].([]any)
	if !ok || len(endpointAddrs) != 1 || endpointAddrs[0] != "http://"+differentialFixturePlaceholder {
		t.Fatalf("endpoint_addrs = %#v", config["endpoint_addrs"])
	}
	if config["token"] != "differential-token" ||
		config["event_transaction_id"] != "${http_x_request_id}" ||
		config["event_subscription_id"] != "differential-subscription" ||
		config["event_code"] != "differential-usage" || config["batch_max_size"] != 1 {
		t.Fatalf("Lago config = %#v", config)
	}
	properties, ok := config["event_properties"].(map[string]any)
	if !ok || properties["route"] != "${route_id}" || properties["status"] != "${status}" {
		t.Fatalf("event_properties = %#v", config["event_properties"])
	}
}
