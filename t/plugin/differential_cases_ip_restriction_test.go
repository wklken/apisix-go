package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialIPRestrictionCasesCoverPinnedAPISIX317LoopbackBlocks(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialIPRestrictionCases()
	if len(cases) != 3 {
		t.Fatalf("differentialIPRestrictionCases() = %d cases, want 3", len(cases))
	}

	byName := make(map[string]DifferentialCase, len(cases))
	for _, spec := range cases {
		if spec.Plugin != "ip-restriction" {
			t.Fatalf("case %q plugin = %q, want ip-restriction", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
			spec.Request.Host != "gateway.example.test" {
			t.Fatalf("case %q request identity = %#v", spec.Name, spec.Request)
		}
		config := string(mustYAML(t, spec.Config))
		for _, want := range []string{"ip-restriction", "127.0.0.0/24", differentialFixturePlaceholder} {
			if !strings.Contains(config, want) {
				t.Fatalf("case %q config does not contain %q:\n%s", spec.Name, want, config)
			}
		}
		if _, duplicate := byName[spec.Name]; duplicate {
			t.Fatalf("duplicate case name %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	// APISIX 3.17 ip-restriction.t TEST 7 creates the loopback CIDR
	// whitelist and TEST 8 proves the request reaches upstream.
	allow := byName["ip-restriction-loopback-cidr-whitelist-allows"]
	if allow.Fixture.ExpectedCalls != 1 || allow.SecurityDecision != "allow" {
		t.Fatalf("whitelist allow semantics = %#v", allow)
	}
	if allow.Fixture.Response.Status != http.StatusOK || allow.Fixture.Response.Body != "hello world" {
		t.Fatalf("whitelist fixture response = %#v", allow.Fixture.Response)
	}

	// APISIX 3.17 ip-restriction.t TEST 12 creates the loopback CIDR
	// blacklist and TEST 13 proves the plugin rejects before upstream.
	deny := byName["ip-restriction-loopback-cidr-blacklist-denies"]
	if deny.Fixture.ExpectedCalls != 0 || deny.SecurityDecision != "deny" {
		t.Fatalf("blacklist deny semantics = %#v", deny)
	}

	// APISIX 3.17 ip-restriction.t TEST 26 configures a custom message and
	// TEST 27 proves that message is returned for the same denied peer.
	custom := byName["ip-restriction-loopback-blacklist-custom-message"]
	if custom.Fixture.ExpectedCalls != 0 || custom.SecurityDecision != "deny" {
		t.Fatalf("custom-message deny semantics = %#v", custom)
	}
	config := string(mustYAML(t, custom.Config))
	if !strings.Contains(config, "Do you want to do something bad?") {
		t.Fatalf("custom-message config missing APISIX 3.17 message:\n%s", config)
	}
}
