package pluginintegration

import (
	"net/http"
	"strings"
	"testing"
)

func TestDifferentialConsumerRestrictionCasesCoverPinnedAPISIX317BasicAuthWhitelist(t *testing.T) {
	if compatibilityOracleSourceCommit != "9ef2ecab67f652d38365049613610ef649bb4ad0" {
		t.Fatalf("compatibility oracle source commit = %q", compatibilityOracleSourceCommit)
	}

	cases := differentialConsumerRestrictionCases()
	if len(cases) != 3 {
		t.Fatalf("differentialConsumerRestrictionCases() = %d cases, want 3", len(cases))
	}

	want := []struct {
		name             string
		authorization    string
		expectedCalls    int
		securityDecision string
	}{
		{
			name:             "consumer-restriction-basic-whitelist-missing-authorization",
			expectedCalls:    0,
			securityDecision: "deny",
		},
		{
			name:             "consumer-restriction-basic-whitelist-jack1-allowed",
			authorization:    "Basic amFjazIwMTk6MTIzNDU2",
			expectedCalls:    1,
			securityDecision: "allow",
		},
		{
			name:             "consumer-restriction-basic-whitelist-jack2-denied",
			authorization:    "Basic amFjazIwMjA6MTIzNDU2",
			expectedCalls:    0,
			securityDecision: "deny",
		},
	}

	seenNames := make(map[string]struct{}, len(cases))
	seenRouteIDs := make(map[string]struct{}, len(cases))
	for i, spec := range cases {
		if spec.Name != want[i].name {
			t.Fatalf("case %d name = %q, want %q", i, spec.Name, want[i].name)
		}
		if _, exists := seenNames[spec.Name]; exists {
			t.Fatalf("duplicate case name %q", spec.Name)
		}
		seenNames[spec.Name] = struct{}{}

		if spec.Plugin != "consumer-restriction" {
			t.Fatalf("case %q plugin = %q, want consumer-restriction", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" || len(spec.RouteID) > 64 {
			t.Fatalf("case %q route ID length = %d, want 1..64", spec.Name, len(spec.RouteID))
		}
		if _, exists := seenRouteIDs[spec.RouteID]; exists {
			t.Fatalf("duplicate route ID %q", spec.RouteID)
		}
		seenRouteIDs[spec.RouteID] = struct{}{}

		if spec.Request.Method != http.MethodGet || spec.Request.Path != "/hello" {
			t.Fatalf(
				"case %q request = %s %s, want GET /hello",
				spec.Name,
				spec.Request.Method,
				spec.Request.Path,
			)
		}
		if got := spec.Request.Headers["Authorization"]; got != want[i].authorization {
			t.Fatalf("case %q Authorization = %q, want %q", spec.Name, got, want[i].authorization)
		}
		if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != want[i].expectedCalls {
			t.Fatalf(
				"case %q fixture = %#v, want primary called %d times",
				spec.Name,
				spec.Fixture,
				want[i].expectedCalls,
			)
		}
		if spec.Fixture.Response.Status != http.StatusOK ||
			spec.Fixture.Response.Body != "hello world" {
			t.Fatalf("case %q fixture response = %#v", spec.Name, spec.Fixture.Response)
		}
		if spec.SecurityDecision != want[i].securityDecision {
			t.Fatalf(
				"case %q security decision = %q, want %q",
				spec.Name,
				spec.SecurityDecision,
				want[i].securityDecision,
			)
		}

		config := string(mustYAML(t, spec.Config))
		for _, token := range []string{
			"jack1", "jack2", "jack2019", "jack2020", `"123456"`,
			"basic-auth", "consumer-restriction", "whitelist", differentialFixturePlaceholder,
		} {
			if !strings.Contains(config, token) {
				t.Fatalf(
					"case %q standalone config does not contain %q:\n%s",
					spec.Name,
					token,
					config,
				)
			}
		}
		if !strings.Contains(config, "- jack1") || strings.Contains(config, "- jack2\n") {
			t.Fatalf("case %q whitelist must contain only jack1:\n%s", spec.Name, config)
		}
		if !strings.Contains(config, "id: "+spec.RouteID) {
			t.Fatalf(
				"case %q config does not use route ID %q:\n%s",
				spec.Name,
				spec.RouteID,
				config,
			)
		}
	}
}
