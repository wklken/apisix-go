package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildStrictSkipsExplicitlyDisabledRoutes(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "live-route", []byte(`{"id":"live-route","uri":"/live-route"}`))
	putRouteResource(t, "disabled-route", []byte(`{"id":"disabled-route","uri":"/disabled-route","status":0}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v), want disabled route skipped", handler, err)
	}

	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/live-route", nil))
	if live.Code == http.StatusNotFound {
		t.Fatalf("enabled route status = %d, want a registered handler", live.Code)
	}

	disabled := httptest.NewRecorder()
	handler.ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, "/disabled-route", nil))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled route status = %d, want 404", disabled.Code)
	}
}

func TestBuildStrictServesOmittedAndExplicitEnabledStatus(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "omitted-status", []byte(`{"id":"omitted-status","uri":"/omitted-status"}`))
	putRouteResource(t, "enabled-status", []byte(`{"id":"enabled-status","uri":"/enabled-status","status":1}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err != nil || handler == nil {
		t.Fatalf("BuildStrict() = (%T, %v)", handler, err)
	}
	for _, path := range []string{"/omitted-status", "/enabled-status"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == http.StatusNotFound {
			t.Fatalf("%s status = %d, want registered", path, response.Code)
		}
	}
}

func TestBuildStrictRejectsUnknownRouteStatus(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "bad-status", []byte(`{"id":"bad-status","uri":"/bad-status","status":2}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want status rejection", handler, err)
	}
	if !strings.Contains(err.Error(), "bad-status") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("BuildStrict() error = %q, want route ID and status", err)
	}
}

func TestBuildStrictRejectsNullRouteStatus(t *testing.T) {
	ensureRouteStore(t)
	setHTTPPluginAllowlist(t)
	putRouteResource(t, "null-status-route", []byte(`{"id":"null-status-route","uri":"/null-status","status":null}`))

	builder := NewBuilder(nil)
	t.Cleanup(builder.Stop)
	handler, err := builder.BuildStrict()
	if err == nil || handler != nil {
		t.Fatalf("BuildStrict() = (%T, %v), want null status rejection", handler, err)
	}
	if !strings.Contains(err.Error(), "null-status-route") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("BuildStrict() error = %q, want route ID and status", err)
	}
}
