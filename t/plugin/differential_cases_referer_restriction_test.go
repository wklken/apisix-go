package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialRefererRestrictionCasesCoverPinnedAPISIX317Blocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialRefererRestrictionCases()
	if len(cases) != 2 {
		t.Fatalf("differentialRefererRestrictionCases() = %d cases, want 2", len(cases))
	}

	byName := make(map[string]DifferentialCase, len(cases))
	for _, spec := range cases {
		if spec.Plugin != "referer-restriction" {
			t.Fatalf("case %q plugin = %q, want referer-restriction", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
			t.Fatalf("case %q has incomplete route/request identity: %#v", spec.Name, spec)
		}
		if len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, exceeds APISIX 3.17 maxLength 64", spec.Name, len(spec.RouteID))
		}
		config := string(mustYAML(t, spec.Config))
		for _, want := range []string{"referer-restriction", "'*.xx.com'", "yy.com", differentialFixturePlaceholder} {
			if !strings.Contains(config, want) {
				t.Fatalf("case %q config does not contain %q:\n%s", spec.Name, want, config)
			}
		}
		if _, exists := byName[spec.Name]; exists {
			t.Fatalf("duplicate case name %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	// APISIX 3.17 referer-restriction.t TEST 1 creates the whitelist and
	// TEST 2 proves that its wildcard entry reaches the upstream.
	allow, ok := byName["referer-restriction-wildcard-whitelist-allows"]
	if !ok {
		t.Fatal("missing APISIX 3.17 referer-restriction.t TEST 1/2 differential case")
	}
	if got := allow.Request.Headers["Referer"]; got != "http://www.xx.com" {
		t.Fatalf("wildcard allow Referer = %q", got)
	}
	if allow.Fixture.ExpectedCalls != 1 || allow.Fixture.Response.Status != http.StatusOK ||
		allow.Fixture.Response.Body != "hello world" || allow.SecurityDecision != "allow" {
		t.Fatalf("wildcard allow semantics = %#v", allow)
	}

	// APISIX 3.17 referer-restriction.t TEST 4 proves that an exact yy.com
	// whitelist entry does not admit a www.yy.com subdomain.
	deny, ok := byName["referer-restriction-exact-whitelist-rejects-subdomain"]
	if !ok {
		t.Fatal("missing APISIX 3.17 referer-restriction.t TEST 4 differential case")
	}
	if got := deny.Request.Headers["Referer"]; got != "https://www.yy.com/am" {
		t.Fatalf("exact-domain deny Referer = %q", got)
	}
	if deny.Fixture.ExpectedCalls != 0 || deny.SecurityDecision != "deny" {
		t.Fatalf("exact-domain deny semantics = %#v", deny)
	}
}
