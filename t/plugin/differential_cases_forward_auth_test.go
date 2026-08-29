package pluginintegration

import (
	"net/http"
	"testing"
)

func assertDifferentialAuthCaseShapeWithPolicy(
	t *testing.T,
	cases []DifferentialCase,
	wantName string,
	wantPlugin string,
	wantFixtureCalls int,
	wantPolicy string,
) DifferentialCase {
	t.Helper()
	if len(cases) != 1 {
		t.Fatalf("case count = %d, want 1", len(cases))
	}
	policy := cases[0].ComparisonPolicy
	cases[0].ComparisonPolicy = ""
	spec := assertDifferentialAuthCaseShape(t, cases, wantName, wantPlugin, wantFixtureCalls)
	if policy != wantPolicy {
		t.Fatalf("comparison policy = %q, want %q", policy, wantPolicy)
	}
	spec.ComparisonPolicy = policy
	return spec
}

func TestDifferentialForwardAuthCasesCoverPinnedAPISIX317DeniedClientHeader(t *testing.T) {
	spec := assertDifferentialAuthCaseShapeWithPolicy(
		t, differentialForwardAuthCases(), "forward-auth-deny-copies-client-header", "forward-auth", 1,
		differentialComparisonForwardAuthEmptyErrorContentType,
	)
	if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
		spec.Request.Headers["Authorization"] != "333" {
		t.Fatalf("request = %#v", spec.Request)
	}
	if spec.Fixture.Response.Status != http.StatusForbidden ||
		spec.Fixture.Response.Headers["Location"] != "http://example.com/auth" {
		t.Fatalf("fixture response = %#v", spec.Fixture.Response)
	}
	route := spec.Config["routes"].([]any)[0].(map[string]any)
	auth := route["plugins"].(map[string]any)["forward-auth"].(map[string]any)
	if auth["uri"] != "http://"+differentialFixturePlaceholder+"/auth" {
		t.Fatalf("forward-auth config = %#v", auth)
	}
}
