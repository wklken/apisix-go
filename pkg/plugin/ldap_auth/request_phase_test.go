package ldap_auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesAuthenticationStateAndLegacyHandlerCallsNextOnce(t *testing.T) {
	addLDAPConsumer(t, "phase-ldap-user", "cn=phase-alice,dc=example,dc=org")
	p := newTestPlugin(t, func(username, password string, cfg Config) error { return nil })
	request := ldapRequest("phase-alice", "secret")

	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state, ok := ctx.AuthenticationStateFrom(result.Request)
	if !ok || state.Source != name || state.Consumer().Username != "phase-ldap-user" {
		t.Fatalf("authentication state = (%+v, %v), want ldap-auth/phase-ldap-user", state, ok)
	}

	nextCalls := 0
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ })).ServeHTTP(
		httptest.NewRecorder(), request,
	)
	if nextCalls != 1 {
		t.Fatalf("legacy handler next calls = %d, want 1", nextCalls)
	}
}

func TestRunRequestPhaseStopsWithoutCallingLegacyNext(t *testing.T) {
	p := newTestPlugin(t, func(username, password string, cfg Config) error { return nil })
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestStop {
		t.Fatalf("RunRequestPhase() decision = %v, want stop", result.Decision)
	}

	nextCalls := 0
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ })).ServeHTTP(
		httptest.NewRecorder(), request,
	)
	if nextCalls != 0 {
		t.Fatalf("legacy handler next calls = %d, want 0", nextCalls)
	}
}
