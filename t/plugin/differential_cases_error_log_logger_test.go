package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialErrorLogLoggerCasesCoverPinnedAPISIX317ClickHouseWarningDelivery(
	t *testing.T,
) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialErrorLogLoggerCases()
	if len(cases) != 1 {
		t.Fatalf("error-log-logger cases = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "error-log-logger-clickhouse-delivers-basic-auth-warning" ||
		spec.Plugin != "error-log-logger" ||
		spec.RouteID != "differential-error-log-logger-clickhouse" ||
		spec.ComparisonPolicy != differentialErrorLogLoggerClickHouseDeliveryPolicy {
		t.Fatalf("case identity = %#v", spec)
	}
	if spec.Request.Method != "" || spec.Request.Path != "" || spec.SecurityDecision != "" {
		t.Fatalf(
			"legacy request fields = %#v/%q, want sequence-only case",
			spec.Request,
			spec.SecurityDecision,
		)
	}
	if len(spec.Steps) != 1 {
		t.Fatalf("steps = %#v, want one request", spec.Steps)
	}
	step := spec.Steps[0]
	if step.Request.Method != http.MethodGet || step.Request.Path != "/warn" ||
		step.Request.Host != "gateway.example.test" ||
		step.Request.Headers["Authorization"] != "Bearer definitely-not-basic" ||
		step.SecurityDecision != "allow" {
		t.Fatalf("warning step = %#v", step)
	}

	metadataList, ok := spec.Config["plugin_metadata"].([]any)
	if !ok || len(metadataList) != 1 {
		t.Fatalf("plugin metadata = %#v", spec.Config["plugin_metadata"])
	}
	metadata, ok := metadataList[0].(map[string]any)
	if !ok || metadata["id"] != "error-log-logger" || metadata["level"] != "WARN" ||
		metadata["batch_max_size"] != 2 || metadata["inactive_timeout"] != 1 ||
		metadata["max_retry_count"] != 0 {
		t.Fatalf("error-log-logger metadata = %#v", metadataList[0])
	}
	clickhouse, ok := metadata["clickhouse"].(map[string]any)
	if !ok ||
		clickhouse["endpoint_addr"] != "http://"+differentialFixturePlaceholder+differentialErrorLogLoggerClickHousePath ||
		clickhouse["user"] != "default" ||
		clickhouse["password"] != "differential-password" ||
		clickhouse["database"] != "default" ||
		clickhouse["logtable"] != "logs" {
		t.Fatalf("ClickHouse metadata = %#v", metadata["clickhouse"])
	}

	consumers, ok := spec.Config["consumers"].([]any)
	if !ok || len(consumers) != 1 {
		t.Fatalf("consumers = %#v", spec.Config["consumers"])
	}
	wantConsumer := map[string]any{"username": "anonymous", "plugins": map[string]any{}}
	if !reflect.DeepEqual(consumers[0], wantConsumer) {
		t.Fatalf("anonymous consumer = %#v, want %#v", consumers[0], wantConsumer)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/warn" {
		t.Fatalf("route = %#v", routes[0])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("route plugins = %#v", route["plugins"])
	}
	wantBasicAuth := map[string]any{"anonymous_consumer": "anonymous", "hide_credentials": true}
	if !reflect.DeepEqual(plugins["basic-auth"], wantBasicAuth) {
		t.Fatalf("basic-auth config = %#v, want %#v", plugins["basic-auth"], wantBasicAuth)
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok || upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("upstream = %#v", route["upstream"])
	}

	wantHeaders := []string{
		"Content-Type", "X-ClickHouse-Database", "X-ClickHouse-Key", "X-ClickHouse-User",
		"X-Consumer-Username",
	}
	if spec.Fixture.Name != "origin-and-error-log-clickhouse" ||
		spec.Fixture.ExpectedCalls != 2 || spec.Fixture.CollectTimeoutMillis != 6000 ||
		spec.Fixture.RequestWindowQuietMillis != 1500 ||
		!reflect.DeepEqual(spec.Fixture.SemanticHeaders, wantHeaders) ||
		spec.Fixture.Response.Status != http.StatusNoContent || spec.Fixture.Response.Body != "" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
}
