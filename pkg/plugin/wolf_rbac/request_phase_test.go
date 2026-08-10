package wolf_rbac

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRunRequestPhasePublishesAuthenticationStateAndLegacyHandlerCallsNextOnce(t *testing.T) {
	wolf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"data": map[string]any{"userInfo": map[string]any{"id": "phase-id", "username": "phase-wolf-user"}},
		})
	}))
	t.Cleanup(wolf.Close)
	identity := fmt.Sprintf("%d", time.Now().UnixNano())
	username := "phase-wolf-user-" + identity
	appid := "phase-app-" + identity
	addWolfConsumer(t, username, appid, wolf.URL)
	p := newTestPlugin(t, Config{})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Authorization", "V1#"+appid+"#token")

	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state, ok := ctx.AuthenticationStateFrom(result.Request)
	if !ok || state.Source != name || state.Consumer().Username != username {
		t.Fatalf("authentication state = (%+v, %v), want wolf-rbac/%s", state, ok, username)
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
