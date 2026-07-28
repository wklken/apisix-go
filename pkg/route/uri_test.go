package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConvertURIToChiPattern(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		uri  string
		want string
	}{
		{name: "literal", uri: "/blog/bar", want: "/blog/bar"},
		{name: "suffix wildcard", uri: "/blog/bar*", want: "/blog/bar*"},
		{name: "embedded wildcard", uri: "/articles/*/comments", want: "/articles/*"},
		{name: "colon parameter", uri: "/blog/:name", want: "/blog/{name}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := convertURI(tt.uri)
			if err != nil {
				t.Fatalf("convertURI(%q) error = %v", tt.uri, err)
			}
			if got != tt.want {
				t.Fatalf("convertURI(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

func TestConvertURIRejectsUnsupportedPatterns(t *testing.T) {
	t.Parallel()

	for _, uri := range []string{
		"",
		"articles/*/comments",
		"/articles/pre*post/comments",
		"/articles/*/comments/*/replies",
		"/articles/:id/*",
		"/articles/:/comments",
		"/articles/{id}/comments",
	} {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()

			if _, err := convertURI(uri); err == nil {
				t.Fatalf("convertURI(%q) error = nil, want unsupported URI error", uri)
			}
		})
	}
}

func TestRegisterRouteMatchesEmbeddedWildcardAcrossPathDepths(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	if err := registerRoute(router, []string{http.MethodGet}, "/articles/*/comments", handler); err != nil {
		t.Fatalf("registerRoute() error = %v", err)
	}

	for _, path := range []string{
		"/articles/12345/comments",
		"/articles/2026/07/comments",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("GET %s status = %d, want %d", path, response.Code, http.StatusNoContent)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/articles/12345/replies", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET non-matching suffix status = %d, want %d", response.Code, http.StatusNotFound)
	}

	request = httptest.NewRequest(http.MethodPost, "/articles/12345/comments", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST method-restricted route status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestRegisterRouteWithoutMethodsUsesConvertedURI(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	if err := registerRoute(router, nil, "/articles/*/comments", handler); err != nil {
		t.Fatalf("registerRoute() error = %v", err)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequest(method, "/articles/12345/comments", nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want %d", method, response.Code, http.StatusNoContent)
		}
	}
}

func TestEmbeddedWildcardDoesNotShadowExactSiblingRoute(t *testing.T) {
	t.Parallel()

	for _, wildcardFirst := range []bool{true, false} {
		name := "exact-first"
		if wildcardFirst {
			name = "wildcard-first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			router := chi.NewRouter()
			wildcard := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusNoContent)
			})
			exact := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusCreated)
			})
			registerWildcard := func() {
				t.Helper()
				if err := registerRoute(router, nil, "/articles/*/comments", wildcard); err != nil {
					t.Fatalf("register wildcard route: %v", err)
				}
			}
			registerExact := func() {
				t.Helper()
				if err := registerRoute(router, nil, "/articles/12345/replies", exact); err != nil {
					t.Fatalf("register exact route: %v", err)
				}
			}
			if wildcardFirst {
				registerWildcard()
				registerExact()
			} else {
				registerExact()
				registerWildcard()
			}

			response := httptest.NewRecorder()
			router.ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "/articles/12345/replies", nil),
			)
			if response.Code != http.StatusCreated {
				t.Fatalf("exact sibling status = %d, want %d", response.Code, http.StatusCreated)
			}
		})
	}
}

func TestRequestContextPreservesOriginalEmbeddedWildcardURI(t *testing.T) {
	t.Parallel()

	config := buildRequestContextConfig(
		resource.Route{ID: "articles", Uri: "/articles/*/comments"},
		resource.Service{},
		nil,
	)
	if got := config["$matched_uri"]; got != "/articles/*/comments" {
		t.Fatalf("$matched_uri = %q, want original APISIX pattern", got)
	}
}
