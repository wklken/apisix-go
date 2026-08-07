package public_api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLookupRequiresExactMethodAndURI(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)

	seen := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	Register(http.MethodGet, "/public", handler)

	if got := Lookup(http.MethodGet, "/public"); got == nil {
		t.Fatal("Lookup(GET /public) = nil, want registered handler")
	}
	if got := Lookup(http.MethodPost, "/public"); got != nil {
		t.Fatal("Lookup(POST /public) = non-nil, want nil for different method")
	}
	if got := Lookup(http.MethodGet, "/other"); got != nil {
		t.Fatal("Lookup(GET /other) = non-nil, want nil for different URI")
	}
}

func TestPublicAPIHandlerDispatch(t *testing.T) {
	ResetRegistryForTest()
	t.Cleanup(ResetRegistryForTest)

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
			Register(http.MethodGet, test.wantPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenPath = r.URL.Path
				w.WriteHeader(http.StatusNoContent)
			}))

			plugin := &Plugin{config: Config{URI: test.configURI}}
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
