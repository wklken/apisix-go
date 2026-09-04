package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestRouteRegistrarRegistersPurgeForExplicitMethods(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{"/cache/:key", "/cache/*"} {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()

			router := chi.NewRouter()
			registrar := newRouteRegistrar(router)
			handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusNoContent)
			})
			if err := registrar.registerRouteWithHosts(
				[]string{http.MethodGet, "PURGE"},
				uri,
				nil,
				handler,
			); err != nil {
				t.Fatalf("register %s: %v", uri, err)
			}

			requestPath := "/cache/item"
			for _, method := range []string{http.MethodGet, "PURGE"} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(method, requestPath, nil))
				if response.Code != http.StatusNoContent {
					t.Fatalf("%s %s status = %d, want %d", method, requestPath, response.Code, http.StatusNoContent)
				}
			}
		})
	}
}

func TestWildcardDispatcherMethodMissUsesAPISIX404(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	registrar := newRouteRegistrar(router)
	register := func(methods []string, pattern string, hosts []string) {
		t.Helper()
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		if err := registrar.registerRouteWithHosts(methods, pattern, hosts, handler); err != nil {
			t.Fatalf("register %s: %v", pattern, err)
		}
	}

	register([]string{http.MethodPost, http.MethodGet, http.MethodPost}, "/items/*", []string{"api.example.com"})
	register([]string{http.MethodDelete}, "/items/*", []string{"other.example.com"})

	request := httptest.NewRequest(http.MethodPut, "/items/123", nil)
	request.Host = "api.example.com"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("PUT /items/123 status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if got := response.Header().Get("Allow"); got != "" {
		t.Fatalf("PUT /items/123 Allow = %q, want empty", got)
	}
}

func TestEmbeddedWildcardMethodMissUsesAPISIX404(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	registrar := newRouteRegistrar(router)
	register := func(methods []string, hosts []string) {
		t.Helper()
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusNoContent)
		})
		if err := registrar.registerRouteWithHosts(methods, "/articles/*/comments", hosts, handler); err != nil {
			t.Fatalf("register route: %v", err)
		}
	}
	register([]string{http.MethodGet}, []string{"api.example.com"})
	register([]string{http.MethodPost}, []string{"*.example.com"})
	register([]string{"PURGE"}, nil)
	register([]string{http.MethodGet}, []string{"api.example.com"})

	request := httptest.NewRequest(http.MethodPut, "/articles/tenant/comments", nil)
	request.Host = "api.example.com"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("PUT /articles/tenant/comments status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if got := response.Header().Get("Allow"); got != "" {
		t.Fatalf("PUT /articles/tenant/comments Allow = %q, want empty", got)
	}
}
