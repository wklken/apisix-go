package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialChaitinWAFCasesCoverPinnedAPISIX317BlockReject(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialChaitinWAFCases()
	if len(cases) != 1 {
		t.Fatalf("differentialChaitinWAFCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "chaitin-waf-block-mode-reject" || spec.Plugin != "chaitin-waf" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID != "differential-chaitin-waf-reject" {
		t.Fatalf("route ID = %q", spec.RouteID)
	}
	if spec.ComparisonPolicy != "chaitin-waf-elapsed-time" {
		t.Fatalf("comparison policy = %q, want narrow elapsed-time policy", spec.ComparisonPolicy)
	}
	if len(spec.Steps) != 0 {
		t.Fatalf("steps = %d, want legacy single request", len(spec.Steps))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.SecurityDecision != "deny" {
		t.Fatalf("security decision = %q, want deny", spec.SecurityDecision)
	}
	if spec.Fixture.Name != "waf" || spec.Fixture.ExpectedCalls != 1 {
		t.Fatalf("fixture = %#v, want one WAF call and no route-upstream call", spec.Fixture)
	}
	if spec.Fixture.WireProtocol != differentialFixtureWireT1KV2 {
		t.Fatalf("fixture wire protocol = %q, want explicit T1K v2", spec.Fixture.WireProtocol)
	}
	if spec.Fixture.Response.Status != http.StatusForbidden ||
		spec.Fixture.Response.Body != `{"status":403,"event_id":"b3c6ce574dc24f09a01f634a39dca83b"}` {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}

	metadataList, ok := spec.Config["plugin_metadata"].([]any)
	if !ok || len(metadataList) != 1 {
		t.Fatalf("plugin metadata = %#v", spec.Config["plugin_metadata"])
	}
	metadata, ok := metadataList[0].(map[string]any)
	if !ok || metadata["id"] != "chaitin-waf" || metadata["mode"] != "block" {
		t.Fatalf("metadata = %#v", metadataList[0])
	}
	nodes, ok := metadata["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("metadata nodes = %#v", metadata["nodes"])
	}
	node, ok := nodes[0].(map[string]any)
	if !ok || node["host"] != differentialFixtureHostPlaceholder ||
		node["port"] != differentialFixturePortPlaceholder {
		t.Fatalf("metadata node = %#v", nodes[0])
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v", spec.Config["routes"])
	}
	route, ok := routes[0].(map[string]any)
	if !ok || route["id"] != spec.RouteID || route["uri"] != "/hello" {
		t.Fatalf("route = %#v", routes[0])
	}
	methods, ok := route["methods"].([]any)
	if !ok || !reflect.DeepEqual(methods, []any{http.MethodGet}) {
		t.Fatalf("methods = %#v, want GET", route["methods"])
	}
	plugins, ok := route["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins = %#v", route["plugins"])
	}
	pluginConfig, ok := plugins["chaitin-waf"].(map[string]any)
	if !ok || pluginConfig["mode"] != "block" {
		t.Fatalf("chaitin-waf config = %#v", plugins["chaitin-waf"])
	}
	wafUpstream, ok := pluginConfig["upstream"].(map[string]any)
	if !ok || !reflect.DeepEqual(wafUpstream["servers"], []any{"httpbun.org"}) {
		t.Fatalf("waf upstream = %#v", pluginConfig["upstream"])
	}
	upstream, ok := route["upstream"].(map[string]any)
	if !ok {
		t.Fatalf("route upstream = %#v", route["upstream"])
	}
	upstreamNodes, ok := upstream["nodes"].(map[string]any)
	if !ok || upstreamNodes[differentialFixturePlaceholder] != 1 {
		t.Fatalf("route upstream nodes = %#v", upstream["nodes"])
	}
}
