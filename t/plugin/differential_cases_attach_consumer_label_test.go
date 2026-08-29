package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialAttachConsumerLabelCasesMatchPinnedAPISIX317Blocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialAttachConsumerLabelCases()
	if len(cases) != 1 {
		t.Fatalf("differentialAttachConsumerLabelCases() = %d cases, want 1", len(cases))
	}

	spec := cases[0]
	if spec.Name != "attach-consumer-label-authenticated-consumer-labels" {
		t.Fatalf("case name = %q", spec.Name)
	}
	if spec.Plugin != "attach-consumer-label" {
		t.Fatalf("plugin = %q, want attach-consumer-label", spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/echo" {
		t.Fatalf("request = %s %s, want GET /echo", spec.Request.Method, spec.Request.Path)
	}
	if got := spec.Request.Headers["apikey"]; got != "key-a" {
		t.Fatalf("apikey = %q, want key-a", got)
	}
	if got := spec.Request.Headers["X-Consumer-Role"]; got != "admin" {
		t.Fatalf("spoofed X-Consumer-Role = %q, want admin", got)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 {
		t.Fatalf("fixture = %#v, want primary called once", spec.Fixture)
	}
	if spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "done" {
		t.Fatalf("fixture response = %#v, want 200 done", spec.Fixture.Response)
	}
	if spec.SecurityDecision != "allow" {
		t.Fatalf("security decision = %q, want allow", spec.SecurityDecision)
	}

	consumers, ok := spec.Config["consumers"].([]any)
	if !ok || len(consumers) != 1 {
		t.Fatalf("consumers = %#v, want one", spec.Config["consumers"])
	}
	consumer := consumers[0].(map[string]any)
	if consumer["username"] != "jack" {
		t.Fatalf("consumer username = %#v, want jack", consumer["username"])
	}
	wantLabels := map[string]any{"department": "devops", "company": "api7"}
	if !reflect.DeepEqual(consumer["labels"], wantLabels) {
		t.Fatalf("consumer labels = %#v, want %#v", consumer["labels"], wantLabels)
	}
	consumerPlugins := consumer["plugins"].(map[string]any)
	if got := consumerPlugins["key-auth"].(map[string]any)["key"]; got != "key-a" {
		t.Fatalf("consumer key-auth key = %#v, want key-a", got)
	}

	routes, ok := spec.Config["routes"].([]any)
	if !ok || len(routes) != 1 {
		t.Fatalf("routes = %#v, want one", spec.Config["routes"])
	}
	route := routes[0].(map[string]any)
	if route["id"] != spec.RouteID || route["uri"] != "/echo" || route["upstream"] == nil {
		t.Fatalf("route identity/upstream = %#v", route)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["type"] != "roundrobin" {
		t.Fatalf("upstream type = %#v, want roundrobin", upstream["type"])
	}
	if got := upstream["nodes"].(map[string]any)[differentialFixturePlaceholder]; got != 1 {
		t.Fatalf("fixture node weight = %#v, want 1", got)
	}
	routePlugins := route["plugins"].(map[string]any)
	if _, ok := routePlugins["key-auth"].(map[string]any); !ok {
		t.Fatalf("route key-auth config = %#v, want object", routePlugins["key-auth"])
	}
	wantHeaders := map[string]any{
		"X-Consumer-Department": "$department",
		"X-Consumer-Company":    "$company",
		"X-Consumer-Role":       "$role",
	}
	attachConfig := routePlugins["attach-consumer-label"].(map[string]any)
	if !reflect.DeepEqual(attachConfig["headers"], wantHeaders) {
		t.Fatalf("attach-consumer-label headers = %#v, want %#v", attachConfig["headers"], wantHeaders)
	}
}

// APISIX 3.17 attach-consumer-label.t TEST 5 configures key-auth and the
// label projection; TEST 8 proves department/company reach the upstream; and
// TEST 9 proves an absent role label removes a spoofed client header. Keeping
// all three projected header names makes those semantics observable.
func TestDifferentialObservationRetainsAttachConsumerLabelHeaders(t *testing.T) {
	headers := differentialSemanticUpstreamHeaders(http.Header{
		"X-Consumer-Department": []string{"devops"},
		"X-Consumer-Company":    []string{"api7"},
		"X-Consumer-Role":       []string{"admin"},
		"X-Unrelated":           []string{"ignore"},
	})
	want := map[string][]string{
		"X-Consumer-Department": {"devops"},
		"X-Consumer-Company":    {"api7"},
		"X-Consumer-Role":       {"admin"},
	}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("semantic upstream headers = %#v, want exactly %#v", headers, want)
	}
}
