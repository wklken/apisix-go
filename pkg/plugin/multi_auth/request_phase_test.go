package multi_auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesWinningChildAuthenticationStateAndLegacyHandlerCallsNextOnce(t *testing.T) {
	addAuthConsumer(t, "phase-multi-user", map[string]any{
		"key-auth": map[string]any{"key": "phase-multi-key"},
	})
	waitForConsumerKey(t, "key-auth", "phase-multi-key")
	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {"header": "apikey"}},
	}})
	request := newMultiAuthRequest()
	request.Header.Set("apikey", "phase-multi-key")

	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state, ok := ctx.AuthenticationStateFrom(result.Request)
	if !ok || state.Source != "key-auth" || state.Consumer().Username != "phase-multi-user" {
		t.Fatalf("authentication state = (%+v, %v), want key-auth/phase-multi-user", state, ok)
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
	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"basic-auth": {}},
		{"key-auth": {}},
	}})
	request := newMultiAuthRequest()
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
