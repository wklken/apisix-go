package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialLogglyCasesCoverPinnedAPISIX317HTTPBulkDelivery(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	spec := onlyDifferentialLoggerCase(t, differentialLogglyCases(), "loggly")
	assertDifferentialLoggerSequence(
		t,
		spec,
		"loggly-http-posts-single-formatted-entry",
		"/logger/loggly",
		2,
	)
	if spec.RouteID != "differential-loggly-http-delivery" ||
		spec.ComparisonPolicy != "loggly-http-fixture-delivery" {
		t.Fatalf("route/policy = %q/%q", spec.RouteID, spec.ComparisonPolicy)
	}

	metadataList, ok := spec.Config["plugin_metadata"].([]any)
	if !ok || len(metadataList) != 1 {
		t.Fatalf("plugin metadata = %#v", spec.Config["plugin_metadata"])
	}
	metadata, ok := metadataList[0].(map[string]any)
	if !ok || metadata["id"] != "loggly" ||
		metadata["host"] != differentialFixturePlaceholder+"/loggly" ||
		metadata["protocol"] != "http" {
		t.Fatalf("loggly metadata = %#v", metadataList[0])
	}
	wantFormat := map[string]any{
		"case": "loggly", "route_id": "$route_id", "timestamp": "$time_iso8601",
	}
	if !reflect.DeepEqual(metadata["log_format"], wantFormat) {
		t.Fatalf("loggly metadata format = %#v, want %#v", metadata["log_format"], wantFormat)
	}

	config := differentialLoggerPluginConfig(t, spec, "loggly")
	if config["customer_token"] != "differential-token" || config["batch_max_size"] != 1 {
		t.Fatalf("loggly route config = %#v", config)
	}
	if _, exists := config["tags"]; exists {
		t.Fatalf("loggly route config pins tags instead of exercising APISIX default: %#v", config)
	}
	if spec.Fixture.Name != "origin-and-loggly" ||
		!reflect.DeepEqual(spec.Fixture.SemanticHeaders, []string{"Content-Type", "X-LOGGLY-TAG"}) ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "fixture-ok" {
		t.Fatalf("loggly fixture = %#v", spec.Fixture)
	}
}
