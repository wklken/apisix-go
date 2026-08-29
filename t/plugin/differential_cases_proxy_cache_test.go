package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialProxyCacheCasesMatchPinnedAPISIX317MemoryMissBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialProxyCacheCases()
	if len(cases) != 1 {
		t.Fatalf("differentialProxyCacheCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "proxy-cache-memory-first-request-miss" || spec.Plugin != "proxy-cache" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world!" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want not_applicable/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["proxy-cache"].(map[string]any)
	if pluginConfig["cache_strategy"] != "memory" || pluginConfig["cache_zone"] != "memory_cache" ||
		pluginConfig["hide_cache_headers"] != false || pluginConfig["cache_ttl"] != 300 {
		t.Fatalf("proxy-cache config = %#v", pluginConfig)
	}
	if !reflect.DeepEqual(pluginConfig["cache_key"], []any{"$host", "$uri"}) ||
		!reflect.DeepEqual(pluginConfig["cache_method"], []any{http.MethodGet}) ||
		!reflect.DeepEqual(pluginConfig["cache_http_status"], []any{http.StatusOK}) {
		t.Fatalf("proxy-cache key/method/status config = %#v", pluginConfig)
	}
	upstream := route["upstream"].(map[string]any)
	if nodes := upstream["nodes"].(map[string]any); len(nodes) != 1 || nodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("proxy-cache upstream nodes = %#v, want fixture placeholder", nodes)
	}
}
