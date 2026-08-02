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

func TestEmbeddedWildcardSiblingRoutesCoexist(t *testing.T) {
	t.Parallel()

	for _, commentsFirst := range []bool{true, false} {
		name := "likes-first"
		if commentsFirst {
			name = "comments-first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			router := chi.NewRouter()
			register := func(pattern string, status int) {
				t.Helper()
				handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.WriteHeader(status)
				})
				if err := registerRoute(router, []string{http.MethodGet}, pattern, handler); err != nil {
					t.Fatalf("register %s: %v", pattern, err)
				}
			}
			if commentsFirst {
				register("/articles/*/comments", http.StatusNoContent)
				register("/articles/*/likes", http.StatusCreated)
			} else {
				register("/articles/*/likes", http.StatusCreated)
				register("/articles/*/comments", http.StatusNoContent)
			}

			for _, test := range []struct {
				path string
				want int
			}{
				{path: "/articles/123/comments", want: http.StatusNoContent},
				{path: "/articles/123/likes", want: http.StatusCreated},
				{path: "/articles/123/shares", want: http.StatusNotFound},
			} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
				if response.Code != test.want {
					t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, test.want)
				}
			}
		})
	}
}

func TestEmbeddedWildcardWinsOverCatchAllSibling(t *testing.T) {
	t.Parallel()

	for _, embeddedFirst := range []bool{true, false} {
		name := "catch-all-first"
		if embeddedFirst {
			name = "embedded-first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			router := chi.NewRouter()
			register := func(pattern string, status int) {
				t.Helper()
				handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.WriteHeader(status)
				})
				if err := registerRoute(router, []string{http.MethodGet}, pattern, handler); err != nil {
					t.Fatalf("register %s: %v", pattern, err)
				}
			}
			if embeddedFirst {
				register("/articles/*/comments", http.StatusNoContent)
				register("/articles/*", http.StatusCreated)
			} else {
				register("/articles/*", http.StatusCreated)
				register("/articles/*/comments", http.StatusNoContent)
			}

			for _, test := range []struct {
				path string
				want int
			}{
				{path: "/articles/123/comments", want: http.StatusNoContent},
				{path: "/articles/123/likes", want: http.StatusCreated},
			} {
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
				if response.Code != test.want {
					t.Fatalf("GET %s status = %d, want %d", test.path, response.Code, test.want)
				}
			}
		})
	}
}

func TestWildcardDispatcherKeepsMethodScopesSeparate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		allPattern string
		getPattern string
		assertions []struct {
			method string
			path   string
			want   int
		}
	}{
		{
			name:       "all-method embedded with GET catch-all",
			allPattern: "/articles/*/comments",
			getPattern: "/articles/*",
			assertions: []struct {
				method string
				path   string
				want   int
			}{
				{method: http.MethodGet, path: "/articles/123/comments", want: http.StatusAccepted},
				{method: http.MethodPost, path: "/articles/123/comments", want: http.StatusAccepted},
				{method: http.MethodGet, path: "/articles/123/likes", want: http.StatusCreated},
				{method: http.MethodPost, path: "/articles/123/likes", want: http.StatusMethodNotAllowed},
			},
		},
		{
			name:       "all-method catch-all with GET embedded",
			allPattern: "/articles/*",
			getPattern: "/articles/*/comments",
			assertions: []struct {
				method string
				path   string
				want   int
			}{
				{method: http.MethodGet, path: "/articles/123/comments", want: http.StatusCreated},
				{method: http.MethodPost, path: "/articles/123/comments", want: http.StatusAccepted},
				{method: http.MethodGet, path: "/articles/123/likes", want: http.StatusAccepted},
				{method: http.MethodPost, path: "/articles/123/likes", want: http.StatusAccepted},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, allFirst := range []bool{true, false} {
				order := "GET-first"
				if allFirst {
					order = "all-method-first"
				}
				t.Run(order, func(t *testing.T) {
					t.Parallel()

					router := chi.NewRouter()
					register := func(methods []string, pattern string, status int) {
						t.Helper()
						handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
							writer.WriteHeader(status)
						})
						if err := registerRoute(router, methods, pattern, handler); err != nil {
							t.Fatalf("register %s: %v", pattern, err)
						}
					}
					if allFirst {
						register(nil, test.allPattern, http.StatusAccepted)
						register([]string{http.MethodGet}, test.getPattern, http.StatusCreated)
					} else {
						register([]string{http.MethodGet}, test.getPattern, http.StatusCreated)
						register(nil, test.allPattern, http.StatusAccepted)
					}

					for _, assertion := range test.assertions {
						response := httptest.NewRecorder()
						router.ServeHTTP(
							response,
							httptest.NewRequest(assertion.method, assertion.path, nil),
						)
						if response.Code != assertion.want {
							t.Fatalf(
								"%s %s status = %d, want %d",
								assertion.method,
								assertion.path,
								response.Code,
								assertion.want,
							)
						}
					}
				})
			}
		})
	}
}

func TestRouteDispatcherSelectsSamePatternByHost(t *testing.T) {
	t.Parallel()

	router := chi.NewRouter()
	register := func(hosts []string, status int) {
		t.Helper()
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(status)
		})
		if err := registerRouteWithHosts(router, nil, "/*", hosts, handler); err != nil {
			t.Fatalf("register hosts %v: %v", hosts, err)
		}
	}
	register([]string{"sp1.local"}, http.StatusCreated)
	register([]string{"*.example.local"}, http.StatusAccepted)
	register([]string{"sp2.local"}, http.StatusNoContent)

	for _, assertion := range []struct {
		host string
		want int
	}{
		{host: "sp1.local", want: http.StatusCreated},
		{host: "sp1.local:9080", want: http.StatusCreated},
		{host: "api.example.local", want: http.StatusAccepted},
		{host: "sp2.local", want: http.StatusNoContent},
		{host: "other.local", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/uri", nil)
		request.Host = assertion.host
		router.ServeHTTP(response, request)
		if response.Code != assertion.want {
			t.Fatalf("Host %s status = %d, want %d", assertion.host, response.Code, assertion.want)
		}
	}
}

func TestRouteDispatcherKeepsExactHostFallbackInBothRegistrationOrders(t *testing.T) {
	t.Parallel()

	for _, hostSpecificFirst := range []bool{true, false} {
		name := "hostless-first"
		if hostSpecificFirst {
			name = "host-specific-first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			router := chi.NewRouter()
			register := func(hosts []string, status int) {
				t.Helper()
				handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.WriteHeader(status)
				})
				if err := registerRouteWithHosts(router, nil, "/login/callback", hosts, handler); err != nil {
					t.Fatalf("register hosts %v: %v", hosts, err)
				}
			}
			if hostSpecificFirst {
				register([]string{"sp1.local"}, http.StatusCreated)
				register(nil, http.StatusAccepted)
			} else {
				register(nil, http.StatusAccepted)
				register([]string{"sp1.local"}, http.StatusCreated)
			}

			for _, assertion := range []struct {
				host string
				want int
			}{
				{host: "sp1.local", want: http.StatusCreated},
				{host: "unrelated.local", want: http.StatusAccepted},
			} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "/login/callback", nil)
				request.Host = assertion.host
				router.ServeHTTP(response, request)
				if response.Code != assertion.want {
					t.Fatalf("Host %s status = %d, want %d", assertion.host, response.Code, assertion.want)
				}
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

func TestPinDecodedRoutePathMatchesEncodedRequestURI(t *testing.T) {
	mux := chi.NewRouter()
	mux.Use(pinDecodedRoutePath)
	mux.Get("/print_uri_detailed", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Get(server.URL + "/print%5Furi%5Fdetailed")
	if err != nil {
		t.Fatalf("send encoded request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for decoded route match", response.StatusCode)
	}
}

func TestWithoutPinEncodedRequestURIDoesNotMatch(t *testing.T) {
	mux := chi.NewRouter()
	mux.Get("/print_uri_detailed", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	response, err := http.Get(server.URL + "/print%5Furi%5Fdetailed")
	if err != nil {
		t.Fatalf("send encoded request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		t.Fatal("status = 200, want no match without pinDecodedRoutePath")
	}
}
