package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialCSRFCasesCoverPinnedAPISIX317CoreAccessBehavior(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialCSRFCases()
	if len(cases) != 2 {
		t.Fatalf("differentialCSRFCases() = %d cases, want 2", len(cases))
	}

	want := []struct {
		name             string
		method           string
		expectedCalls    int
		securityDecision string
	}{
		{
			name:             "csrf-safe-get-issues-cookie",
			method:           http.MethodGet,
			expectedCalls:    1,
			securityDecision: "allow",
		},
		{
			name:             "csrf-post-missing-token-rejected",
			method:           http.MethodPost,
			expectedCalls:    0,
			securityDecision: "deny",
		},
	}

	seenRouteIDs := make(map[string]struct{}, len(cases))
	for i, expected := range want {
		spec := cases[i]
		if spec.Name != expected.name {
			t.Fatalf("case %d name = %q, want %q", i, spec.Name, expected.name)
		}
		if spec.Plugin != "csrf" {
			t.Fatalf("case %q plugin = %q, want csrf", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if _, duplicate := seenRouteIDs[spec.RouteID]; duplicate {
			t.Fatalf("duplicate route ID %q", spec.RouteID)
		}
		seenRouteIDs[spec.RouteID] = struct{}{}

		if spec.Request.Method != expected.method || spec.Request.Path != "/hello" {
			t.Fatalf(
				"case %q request = %s %s, want %s /hello",
				spec.Name,
				spec.Request.Method,
				spec.Request.Path,
				expected.method,
			)
		}
		if spec.Request.Host != "gateway.example.test" {
			t.Fatalf("case %q Host = %q", spec.Name, spec.Request.Host)
		}
		if len(spec.Request.Headers) != 0 {
			t.Fatalf("case %q headers = %#v, want no CSRF token", spec.Name, spec.Request.Headers)
		}
		if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != expected.expectedCalls {
			t.Fatalf(
				"case %q fixture = %#v, want primary called %d times",
				spec.Name,
				spec.Fixture,
				expected.expectedCalls,
			)
		}
		if spec.SecurityDecision != expected.securityDecision {
			t.Fatalf(
				"case %q security decision = %q, want %q",
				spec.Name,
				spec.SecurityDecision,
				expected.securityDecision,
			)
		}
		if spec.ComparisonPolicy != differentialCSRFIssuedCookieComparisonPolicy {
			t.Fatalf("case %q comparison policy = %q", spec.Name, spec.ComparisonPolicy)
		}

		routes, ok := spec.Config["routes"].([]any)
		if !ok || len(routes) != 1 {
			t.Fatalf("case %q routes = %#v, want one route", spec.Name, spec.Config["routes"])
		}
		route, ok := routes[0].(map[string]any)
		if !ok {
			t.Fatalf("case %q route = %#v, want map", spec.Name, routes[0])
		}
		if route["id"] != spec.RouteID || route["uri"] != "/hello" {
			t.Fatalf("case %q route identity = %#v", spec.Name, route)
		}
		plugins, ok := route["plugins"].(map[string]any)
		if !ok {
			t.Fatalf("case %q plugins = %#v, want map", spec.Name, route["plugins"])
		}
		csrf, ok := plugins["csrf"].(map[string]any)
		if !ok || csrf["key"] != "userkey" || csrf["expires"] != 1000000000 {
			t.Fatalf("case %q csrf config = %#v", spec.Name, plugins["csrf"])
		}
		upstream, ok := route["upstream"].(map[string]any)
		if !ok {
			t.Fatalf("case %q upstream = %#v, want map", spec.Name, route["upstream"])
		}
		nodes, ok := upstream["nodes"].(map[string]any)
		if !ok || nodes[differentialFixturePlaceholder] != 1 {
			t.Fatalf("case %q upstream nodes = %#v, want fixture placeholder", spec.Name, upstream["nodes"])
		}
	}
}

func TestDifferentialCSRFCookiePolicyIsNarrowAndExplicit(t *testing.T) {
	if differentialCSRFIssuedCookieComparisonPolicy != "csrf-issued-cookie" {
		t.Fatalf(
			"CSRF comparison policy = %q, want stable narrow policy name",
			differentialCSRFIssuedCookieComparisonPolicy,
		)
	}
}
