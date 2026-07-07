package redirect

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRedirectsWithRegexURI(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexUri: []string{`^/users/(\d+)$`, `/profiles/$1`},
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/users/42?ignored=1", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusFound {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusFound)
	}
	if got := res.Header().Get("Location"); got != "/profiles/42" {
		t.Fatalf("Location = %q, want /profiles/42", got)
	}
}

func TestHandlerRegexURINoMatchFallsThrough(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexUri: []string{`^/users/(\d+)$`, `/profiles/$1`},
	})
	called := false
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandlerEncodesRegexURIPath(t *testing.T) {
	encodeURI := true
	p := newTestPlugin(t, Config{
		RegexUri:  []string{`^/old$`, `/new path/中文?keep=1`},
		EncodeUri: &encodeURI,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/old", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "/new%20path/%E4%B8%AD%E6%96%87?keep=1" {
		t.Fatalf("Location = %q, want encoded regex redirect URI", got)
	}
}

func TestHandlerEncodesRedirectURIPath(t *testing.T) {
	encodeURI := true
	p := newTestPlugin(t, Config{
		Uri:       "/new path/中文?keep=1",
		EncodeUri: &encodeURI,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/old", nil)
	req.Host = "example.com"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "http://example.com/new%20path/%E4%B8%AD%E6%96%87?keep=1" {
		t.Fatalf("Location = %q, want encoded redirect URI", got)
	}
}
