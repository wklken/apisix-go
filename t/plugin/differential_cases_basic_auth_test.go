package pluginintegration

import (
	"net/http"
	"testing"
)

func TestDifferentialBasicAuthCasesHaveRunnableShape(t *testing.T) {
	cases := differentialBasicAuthCases()
	if len(cases) != 2 {
		t.Fatalf("differentialBasicAuthCases() = %d cases, want 2", len(cases))
	}

	wantNames := []string{
		"basic-auth-hide-credentials",
		"basic-auth-preserve-credentials",
	}
	seenNames := make(map[string]struct{}, len(cases))
	seenRouteIDs := make(map[string]struct{}, len(cases))
	for i, spec := range cases {
		if spec.Name != wantNames[i] {
			t.Errorf("case %d name = %q, want %q", i, spec.Name, wantNames[i])
		}
		if _, duplicate := seenNames[spec.Name]; duplicate {
			t.Errorf("duplicate case name %q", spec.Name)
		}
		seenNames[spec.Name] = struct{}{}

		if spec.Plugin != "basic-auth" {
			t.Errorf("case %q plugin = %q, want basic-auth", spec.Name, spec.Plugin)
		}
		if spec.RouteID == "" {
			t.Errorf("case %q has an empty route ID", spec.Name)
		}
		if _, duplicate := seenRouteIDs[spec.RouteID]; duplicate {
			t.Errorf("duplicate route ID %q", spec.RouteID)
		}
		seenRouteIDs[spec.RouteID] = struct{}{}

		if spec.Request.Method != http.MethodGet || spec.Request.Path != "/echo" {
			t.Errorf("case %q request = %s %s, want GET /echo", spec.Name, spec.Request.Method, spec.Request.Path)
		}
		if spec.Fixture.Name != "primary" || spec.Fixture.ExpectedCalls != 1 {
			t.Errorf("case %q fixture = %#v, want primary called once", spec.Name, spec.Fixture)
		}
		if spec.SecurityDecision != "allow" {
			t.Errorf("case %q security decision = %q, want allow", spec.Name, spec.SecurityDecision)
		}
	}
}

func TestDifferentialBasicAuthCasesMatchAPISIX317CredentialForwarding(t *testing.T) {
	const authorization = "Basic Zm9vOmJhcg=="

	tests := []struct {
		name            string
		hideCredentials bool
	}{
		{name: "basic-auth-hide-credentials", hideCredentials: true},
		{name: "basic-auth-preserve-credentials", hideCredentials: false},
	}

	cases := differentialBasicAuthCases()
	if len(cases) != len(tests) {
		t.Fatalf("differentialBasicAuthCases() = %d cases, want %d", len(cases), len(tests))
	}
	for i, tt := range tests {
		spec := cases[i]
		if spec.Name != tt.name {
			t.Fatalf("case %d name = %q, want %q", i, spec.Name, tt.name)
		}
		if got := spec.Request.Headers["Authorization"]; got != authorization {
			t.Errorf("case %q Authorization = %q, want %q", spec.Name, got, authorization)
		}

		consumers, ok := spec.Config["consumers"].([]any)
		if !ok || len(consumers) != 1 {
			t.Fatalf("case %q consumers = %#v, want one consumer", spec.Name, spec.Config["consumers"])
		}
		consumer := consumers[0].(map[string]any)
		consumerAuth := consumer["plugins"].(map[string]any)["basic-auth"].(map[string]any)
		if consumerAuth["username"] != "foo" || consumerAuth["password"] != "bar" {
			t.Errorf("case %q consumer credentials = %#v, want foo/bar", spec.Name, consumerAuth)
		}

		routes, ok := spec.Config["routes"].([]any)
		if !ok || len(routes) != 1 {
			t.Fatalf("case %q routes = %#v, want one route", spec.Name, spec.Config["routes"])
		}
		route := routes[0].(map[string]any)
		routeAuth := route["plugins"].(map[string]any)["basic-auth"].(map[string]any)
		if got := routeAuth["hide_credentials"]; got != tt.hideCredentials {
			t.Errorf("case %q hide_credentials = %#v, want %t", spec.Name, got, tt.hideCredentials)
		}
		if route["upstream"] == nil {
			t.Errorf("case %q has no upstream", spec.Name)
		}
	}
}

func TestDifferentialObservationRetainsAuthorizationForCredentialForwarding(t *testing.T) {
	headers := differentialSemanticUpstreamHeaders(http.Header{
		"Authorization": []string{"Basic Zm9vOmJhcg=="},
		"Connection":    []string{"close"},
	})
	if got := http.Header(headers).Get("Authorization"); got != "Basic Zm9vOmJhcg==" {
		t.Fatalf("semantic Authorization = %q", got)
	}
	if got := http.Header(headers).Get("Connection"); got != "" {
		t.Fatalf("transport-only Connection unexpectedly retained: %q", got)
	}
}
