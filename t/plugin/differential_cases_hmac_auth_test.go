package pluginintegration

import (
	"net/http"
	"testing"
)

func assertDifferentialAuthCaseShape(
	t *testing.T,
	cases []DifferentialCase,
	wantName string,
	wantPlugin string,
	wantFixtureCalls int,
) DifferentialCase {
	t.Helper()
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}
	if len(cases) != 1 {
		t.Fatalf("case count = %d, want 1", len(cases))
	}
	spec := cases[0]
	if spec.Name != wantName || spec.Plugin != wantPlugin {
		t.Fatalf("case identity = %q/%q, want %q/%q", spec.Name, spec.Plugin, wantName, wantPlugin)
	}
	if spec.RouteID == "" || len(spec.RouteID) > 64 {
		t.Fatalf("route ID %q has length %d, want 1..64", spec.RouteID, len(spec.RouteID))
	}
	if spec.Request.Method == "" || spec.Request.Path == "" || spec.Request.Host != "gateway.example.test" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != wantFixtureCalls {
		t.Fatalf("fixture = %#v, want primary called %d times", spec.Fixture, wantFixtureCalls)
	}
	if spec.SecurityDecision != "deny" || spec.ComparisonPolicy != "" {
		t.Fatalf("decision/policy = %q/%q, want deny/exact", spec.SecurityDecision, spec.ComparisonPolicy)
	}
	routes := spec.Config["routes"].([]any)
	route := routes[0].(map[string]any)
	if route["id"] != spec.RouteID {
		t.Fatalf("configured route ID = %#v, want %q", route["id"], spec.RouteID)
	}
	plugins := route["plugins"].(map[string]any)
	if _, ok := plugins[wantPlugin]; !ok {
		t.Fatalf("route plugins = %#v, want %q", plugins, wantPlugin)
	}
	upstream := route["upstream"].(map[string]any)
	if upstream["nodes"].(map[string]any)[differentialFixturePlaceholder] != 1 {
		t.Fatalf("route upstream = %#v, want deterministic fixture", upstream)
	}
	return spec
}

func TestDifferentialHMACAuthCasesCoverPinnedAPISIX317MissingAuthorization(t *testing.T) {
	spec := assertDifferentialAuthCaseShape(
		t, differentialHMACAuthCases(), "hmac-auth-missing-authorization", "hmac-auth", 0,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
		t.Fatalf("request = %#v, want GET /hello", spec.Request)
	}
	if spec.Request.Headers["Authorization"] != "" {
		t.Fatalf("Authorization unexpectedly set: %#v", spec.Request.Headers)
	}
	consumers := spec.Config["consumers"].([]any)
	consumer := consumers[0].(map[string]any)
	auth := consumer["plugins"].(map[string]any)["hmac-auth"].(map[string]any)
	if auth["key_id"] != "my-access-key" || auth["secret_key"] != "my-secret-key" {
		t.Fatalf("consumer hmac-auth config = %#v", auth)
	}
}
