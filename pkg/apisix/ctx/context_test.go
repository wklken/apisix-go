package ctx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestAttachConsumerSetsUpstreamUsernameHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = WithApisixVars(req, map[string]string{})

	AttachConsumer(req, resource.Consumer{Username: "bob"})

	if got := req.Header.Get("X-Consumer-Username"); got != "bob" {
		t.Fatalf("X-Consumer-Username = %q, want bob", got)
	}
}

func TestRunConsumerPluginsUsesRegisteredRunner(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	called := false
	req = WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		called = true
		next.ServeHTTP(w, r)
	})
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if !called {
		t.Fatal("consumer plugin runner was not called")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestRunConsumerPluginsFallsBackToNextHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}
