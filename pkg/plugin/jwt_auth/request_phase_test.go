package jwt_auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesAuthenticationStateAndLegacyHandlerCallsNextOnce(t *testing.T) {
	addJWTConsumer(t, "phase-jwt-user", "phase-jwt-key", "phase-jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "phase-jwt-secret", map[string]any{
		"key": "phase-jwt-key", "exp": time.Now().Add(time.Hour).Unix(),
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Authorization", "Bearer "+token)

	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state, ok := ctx.AuthenticationStateFrom(result.Request)
	if !ok || state.Source != name || state.Consumer().Username != "phase-jwt-user" {
		t.Fatalf("authentication state = (%+v, %v), want jwt-auth/phase-jwt-user", state, ok)
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
	p := newTestPlugin(t, Config{})
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
