package public_api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestRegistriesIsolateSameMethodAndURI(t *testing.T) {
	first := NewRegistry()
	second := NewRegistry()

	firstHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	secondHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	first.Register(http.MethodGet, "/same", firstHandler)
	second.Register(http.MethodGet, "/same", secondHandler)

	if got := first.Lookup(http.MethodGet, "/same"); got == nil {
		t.Fatal("first registry lost its handler")
	} else if response := serveRegistryHandler(got); response.Code != http.StatusCreated {
		t.Fatalf("first registry status = %d, want 201", response.Code)
	}
	if got := second.Lookup(http.MethodGet, "/same"); got == nil {
		t.Fatal("second registry lost its handler")
	} else if response := serveRegistryHandler(got); response.Code != http.StatusAccepted {
		t.Fatalf("second registry status = %d, want 202", response.Code)
	}
}

func serveRegistryHandler(handler http.Handler) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/same", nil))
	return response
}

func TestLookupRequiresExactMethodAndURI(t *testing.T) {
	registry := NewRegistry()

	seen := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	registry.Register(http.MethodGet, "/public", handler)

	if got := registry.Lookup(http.MethodGet, "/public"); got == nil {
		t.Fatal("Lookup(GET /public) = nil, want registered handler")
	}
	if got := registry.Lookup(http.MethodPost, "/public"); got != nil {
		t.Fatal("Lookup(POST /public) = non-nil, want nil for different method")
	}
	if got := registry.Lookup(http.MethodGet, "/other"); got != nil {
		t.Fatal("Lookup(GET /other) = non-nil, want nil for different URI")
	}
}

func TestPublicAPIHandlerDispatch(t *testing.T) {
	registry := NewRegistry()

	tests := []struct {
		name       string
		configURI  string
		requestURI string
		wantPath   string
		wantCode   int
	}{
		{
			name:       "configured override",
			configURI:  "/internal/status",
			requestURI: "/public",
			wantPath:   "/internal/status",
			wantCode:   http.StatusNoContent,
		},
		{name: "incoming path fallback", requestURI: "/public", wantPath: "/public", wantCode: http.StatusNoContent},
		{name: "unregistered", requestURI: "/missing", wantCode: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var seenPath string
			registry.Register(
				http.MethodGet,
				test.wantPath,
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seenPath = r.URL.Path
					w.WriteHeader(http.StatusNoContent)
				}),
			)

			plugin := &Plugin{config: Config{URI: test.configURI}, registry: registry}
			request := httptest.NewRequest(http.MethodGet, test.requestURI, nil)
			response := httptest.NewRecorder()
			plugin.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

			if response.Code != test.wantCode {
				t.Fatalf("status = %d, want %d", response.Code, test.wantCode)
			}
			if test.wantCode == http.StatusNoContent &&
				(seenPath != test.wantPath || request.URL.Path != test.requestURI) {
				t.Fatalf(
					"seen/original = %q/%q, want %q/%q",
					seenPath,
					request.URL.Path,
					test.wantPath,
					test.requestURI,
				)
			}
		})
	}
}

func TestRunRequestPhasePublishesAPISIXSourceForGatewayMiss(t *testing.T) {
	plugin := &Plugin{registry: NewRegistry()}
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := plugin.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("result = %+v, want apisix stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("source = %q, want apisix", lifecycle.ResponseSource())
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}
