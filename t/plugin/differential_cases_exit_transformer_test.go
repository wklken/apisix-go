package pluginintegration

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDifferentialExitTransformerCasesMatchPinnedAPISIX317TableBodyBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialExitTransformerCases()
	if len(cases) != 1 {
		t.Fatalf("differentialExitTransformerCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "exit-transformer-missing-api-key-json" || spec.Plugin != "exit-transformer" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" || len(spec.Request.Headers) != 0 {
		t.Fatalf("request = %#v, want missing-key GET /hello", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "deny" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want deny/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}
	if got, want := differentialRequiredPluginNames(
		cases,
	), []string{
		"exit-transformer",
		"key-auth",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("required plugins = %v, want %v", got, want)
	}

	consumers := spec.Config["consumers"].([]any)
	consumer := consumers[0].(map[string]any)
	keyAuth := consumer["plugins"].(map[string]any)["key-auth"].(map[string]any)
	if consumer["username"] != "jack" || keyAuth["key"] != "auth-one" {
		t.Fatalf("consumer = %#v", consumer)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	plugins := route["plugins"].(map[string]any)
	if _, ok := plugins["key-auth"]; !ok {
		t.Fatalf("route plugins = %#v, want key-auth collaborator", plugins)
	}
	transformer := plugins["exit-transformer"].(map[string]any)
	if !reflect.DeepEqual(transformer["functions"], []any{differentialExitTransformerMissingAPIKeyFunction}) {
		t.Fatalf("exit-transformer functions = %#v", transformer["functions"])
	}
	if !strings.Contains(
		differentialExitTransformerMissingAPIKeyFunction,
		`return 400, {message = "authentication Failed"}`,
	) ||
		!strings.Contains(differentialExitTransformerMissingAPIKeyFunction, `["content-type"] = "application/json"`) {
		t.Fatalf(
			"exit-transformer function does not retain pinned status/body/header contract:\n%s",
			differentialExitTransformerMissingAPIKeyFunction,
		)
	}
}
