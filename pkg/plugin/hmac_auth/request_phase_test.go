package hmac_auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesAuthenticationStateAndLegacyHandlerCallsNextOnce(t *testing.T) {
	addHMACConsumer(t, "phase-hmac-user", "phase-hmac-key", "phase-hmac-secret")
	p := newTestPlugin(t, Config{})
	date := time.Now().UTC().Format(http.TimeFormat)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Date", date)
	request.Header.Set("Authorization", signatureHeader(
		t,
		"phase-hmac-key",
		"phase-hmac-secret",
		"hmac-sha256",
		[]string{"date"},
		map[string]string{"date": date},
	))

	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state, ok := ctx.AuthenticationStateFrom(result.Request)
	if !ok || state.Source != name || state.Consumer().Username != "phase-hmac-user" {
		t.Fatalf("authentication state = (%+v, %v), want hmac-auth/phase-hmac-user", state, ok)
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
