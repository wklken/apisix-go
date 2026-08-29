package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialRealIPCasesCoverPinnedAPISIX317TrustedPeerBoundary(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialRealIPCases()
	if len(cases) != 2 {
		t.Fatalf("differentialRealIPCases() = %d cases, want 2", len(cases))
	}

	byName := make(map[string]DifferentialCase, len(cases))
	for _, spec := range cases {
		if spec.Plugin != "real-ip" {
			t.Fatalf("case %q plugin = %q, want real-ip", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" ||
			spec.Request.Host != "gateway.example.test" {
			t.Fatalf("case %q request identity = %#v", spec.Name, spec.Request)
		}
		if got := spec.Request.Headers["XFF"]; got != "1.1.1.1" {
			t.Fatalf("case %q XFF = %q, want 1.1.1.1", spec.Name, got)
		}
		if spec.ComparisonPolicy != "" {
			t.Fatalf("case %q comparison policy = %q, want exact", spec.Name, spec.ComparisonPolicy)
		}
		if spec.Fixture.Name != "primary" || spec.Fixture.Response.Status != http.StatusOK ||
			spec.Fixture.Response.Body != "hello world" {
			t.Fatalf("case %q fixture = %#v", spec.Name, spec.Fixture)
		}

		config := string(mustYAML(t, spec.Config))
		for _, want := range []string{
			"real-ip", "http_xff", "ip-restriction", "1.1.1.1",
			differentialFixturePlaceholder,
		} {
			if !strings.Contains(config, want) {
				t.Fatalf("case %q config does not contain %q:\n%s", spec.Name, want, config)
			}
		}
		if _, duplicate := byName[spec.Name]; duplicate {
			t.Fatalf("duplicate case name %q", spec.Name)
		}
		byName[spec.Name] = spec
	}

	// APISIX 3.17 real-ip.t TEST 2/3 proves the http_xff source mapping;
	// TEST 20/21 proves the trusted-loopback boundary that lets the mapped
	// address reach ip-restriction and upstream.
	trusted := byName["real-ip-trusted-peer-rewrites-from-forwarded-for"]
	if trusted.Fixture.ExpectedCalls != 1 || trusted.SecurityDecision != "allow" {
		t.Fatalf("trusted-peer semantics = %#v", trusted)
	}
	trustedConfig := string(mustYAML(t, trusted.Config))
	if !strings.Contains(trustedConfig, "127.0.0.0/24") {
		t.Fatalf("trusted-peer config does not trust loopback:\n%s", trustedConfig)
	}

	// APISIX 3.17 real-ip.t TEST 18 excludes the loopback peer from the
	// plugin trust list and TEST 19 proves that the same forwarded address is
	// ignored, so ip-restriction rejects before upstream.
	untrusted := byName["real-ip-untrusted-peer-ignores-forwarded-for"]
	if untrusted.Fixture.ExpectedCalls != 0 || untrusted.SecurityDecision != "deny" {
		t.Fatalf("untrusted-peer semantics = %#v", untrusted)
	}
	untrustedConfig := string(mustYAML(t, untrusted.Config))
	if !strings.Contains(untrustedConfig, "192.128.0.0/16") ||
		strings.Contains(untrustedConfig, "127.0.0.0/24") {
		t.Fatalf("untrusted-peer config does not preserve APISIX trust boundary:\n%s", untrustedConfig)
	}
}
