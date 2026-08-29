package pluginintegration

import (
	"net/http"
	"reflect"
	"testing"
)

func TestDifferentialErrorPageCasesMatchPinnedAPISIX317Custom500Blocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialErrorPageCases()
	if len(cases) != 1 {
		t.Fatalf("differentialErrorPageCases() = %d cases, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != "error-page-custom-500-body" || spec.Plugin != "error-page" {
		t.Fatalf("case identity = %q/%q", spec.Name, spec.Plugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 0 {
		t.Fatalf("fixture = %#v, want primary not called", spec.Fixture)
	}
	if spec.SecurityDecision != "not_applicable" || spec.ComparisonPolicy != "error-page-charset-parameter" {
		t.Fatalf(
			"decision/policy = %q/%q, want not_applicable/error-page-charset-parameter",
			spec.SecurityDecision,
			spec.ComparisonPolicy,
		)
	}
	if got, want := differentialRequiredPluginNames(
		cases,
	), []string{
		"error-page",
		"serverless-post-function",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("required plugins = %v, want %v", got, want)
	}

	route := spec.Config["routes"].([]any)[0].(map[string]any)
	serverless := route["plugins"].(map[string]any)["serverless-post-function"].(map[string]any)
	if !reflect.DeepEqual(serverless["functions"], []any{
		"return function(conf, ctx) return 500, 'openresty' end",
	}) {
		t.Fatalf("serverless trigger = %#v", serverless["functions"])
	}
	global := spec.Config["global_rules"].([]any)[0].(map[string]any)
	if _, ok := global["plugins"].(map[string]any)["error-page"]; !ok {
		t.Fatalf("global rule plugins = %#v, want error-page", global["plugins"])
	}
	metadata := spec.Config["plugin_metadata"].([]any)[0].(map[string]any)
	page := metadata["error_500"].(map[string]any)
	if metadata["id"] != "error-page" || metadata["enable"] != true ||
		page["body"] != "<html><body><h1>500 Internal Server Error</h1></body></html>" {
		t.Fatalf("error-page metadata = %#v", metadata)
	}
}
