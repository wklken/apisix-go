package ctx

import (
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestBeforeProxyHooksRunInRegistrationOrder(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/original", nil)
	var calls []string
	req = WithBeforeProxyHook(req, func(r *http.Request) {
		calls = append(calls, "first:"+r.URL.Path)
	})
	req = WithBeforeProxyHook(req, func(r *http.Request) {
		calls = append(calls, "second:"+r.URL.Path)
	})
	req.URL.Path = "/final"

	RunBeforeProxyHooks(req)
	RunBeforeProxyHooks(req)

	if got, want := calls, []string{"first:/final", "second:/final"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hook calls = %#v, want %#v", got, want)
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

func TestRunConsumerPluginsRunsRunnerOnceAcrossStackedAuthCalls(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	runnerCalls := 0
	req = WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		runnerCalls++
		next.ServeHTTP(w, r)
	})
	downstreamCalls := 0
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	secondAuth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		RunConsumerPlugins(w, r, downstream)
	})
	response := httptest.NewRecorder()

	RunConsumerPlugins(response, req, secondAuth)

	if runnerCalls != 1 {
		t.Fatalf("consumer runner calls = %d, want 1", runnerCalls)
	}
	if downstreamCalls != 1 {
		t.Fatalf("downstream calls = %d, want 1", downstreamCalls)
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusNoContent)
	}
}
