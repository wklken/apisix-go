package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompileHTTPSkipsExplicitlyDisabledRoutes(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(
			testRouteFromJSON(t, `{"id":"live-route","uri":"/live-route"}`),
			testRouteFromJSON(t, `{"id":"disabled-route","uri":"/disabled-route","status":0}`),
		),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}

	live := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/live-route", nil))
	if live.Code != http.StatusNoContent {
		t.Fatalf("enabled route status = %d, want %d", live.Code, http.StatusNoContent)
	}

	disabled := httptest.NewRecorder()
	snapshot.Handler().ServeHTTP(disabled, httptest.NewRequest(http.MethodGet, "/disabled-route", nil))
	if disabled.Code != http.StatusNotFound {
		t.Fatalf("disabled route status = %d, want %d", disabled.Code, http.StatusNotFound)
	}
}

func TestCompileHTTPServesOmittedAndExplicitEnabledStatus(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(
			testRouteFromJSON(t, `{"id":"omitted-status","uri":"/omitted-status"}`),
			testRouteFromJSON(t, `{"id":"enabled-status","uri":"/enabled-status","status":1}`),
		),
	})
	if err != nil {
		t.Fatalf("CompileHTTP() error = %v", err)
	}
	for _, path := range []string{"/omitted-status", "/enabled-status"} {
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want %d", path, response.Code, http.StatusNoContent)
		}
	}
}

func TestCompileHTTPRejectsUnknownRouteStatus(t *testing.T) {
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: testPreparedRoutes(
			testRouteFromJSON(t, `{"id":"bad-status","uri":"/bad-status","status":2}`),
		),
	})
	if err == nil || snapshot != nil {
		t.Fatalf("CompileHTTP() = (%T, %v), want status rejection", snapshot, err)
	}
	if !strings.Contains(err.Error(), "bad-status") || !strings.Contains(err.Error(), "status") {
		t.Fatalf("CompileHTTP() error = %q, want route ID and status", err)
	}
}
