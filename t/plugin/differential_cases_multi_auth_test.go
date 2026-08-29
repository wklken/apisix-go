package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialMultiAuthCasesCoverPinnedAPISIX317FirstSuccessfulChild(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialMultiAuthCases()
	if len(cases) != 1 {
		t.Fatalf("differentialMultiAuthCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "multi-auth-basic-wins-over-valid-key" || spec.Plugin != "multi-auth" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["Authorization"] != "Basic Zm9vOmJhcg==" ||
		spec.Request.Headers["apikey"] != "auth-one" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 ||
		spec.Fixture.Response.Status != http.StatusOK || spec.Fixture.Response.Body != "hello world" {
		t.Fatalf("fixture = %#v", spec.Fixture)
	}
	if spec.SecurityDecision != "allow" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want allow/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}

	consumers := spec.Config["consumers"].([]any)
	if len(consumers) != 2 {
		t.Fatalf("consumers = %#v, want two discriminating consumers", consumers)
	}
	basicConsumer := consumers[0].(map[string]any)
	keyConsumer := consumers[1].(map[string]any)
	if basicConsumer["username"] != "basic-user" || keyConsumer["username"] != "key-user" {
		t.Fatalf("consumer identities = %#v / %#v", basicConsumer, keyConsumer)
	}
	basicConfig := basicConsumer["plugins"].(map[string]any)["basic-auth"].(map[string]any)
	if basicConfig["username"] != "foo" || basicConfig["password"] != "bar" {
		t.Fatalf("basic consumer config = %#v", basicConfig)
	}
	keyConfig := keyConsumer["plugins"].(map[string]any)["key-auth"].(map[string]any)
	if keyConfig["key"] != "auth-one" {
		t.Fatalf("key consumer config = %#v", keyConfig)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	pluginConfig := route["plugins"].(map[string]any)["multi-auth"].(map[string]any)
	wantAuthPlugins := []any{
		map[string]any{"basic-auth": map[string]any{}},
		map[string]any{"key-auth": map[string]any{
			"query": "apikey", "header": "apikey",
		}},
	}
	if !reflect.DeepEqual(pluginConfig["auth_plugins"], wantAuthPlugins) {
		t.Fatalf("multi-auth children = %#v, want %#v", pluginConfig["auth_plugins"], wantAuthPlugins)
	}
}

func TestDifferentialMultiAuthObservationRetainsWinningConsumerUsername(t *testing.T) {
	headers := differentialSemanticUpstreamHeaders(http.Header{
		"X-Consumer-Username": []string{"basic-user"},
		"apikey":              []string{"auth-one"},
	})
	want := map[string][]string{"X-Consumer-Username": {"basic-user"}}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("semantic upstream headers = %#v, want %#v", headers, want)
	}
}
