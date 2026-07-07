package referer_restriction

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWhitelistRejectsWithJSONMessage(t *testing.T) {
	p := newTestPlugin(t, Config{Whitelist: []string{"allowed.example.com"}})
	req := httptest.NewRequest(http.MethodGet, "/restricted", nil)
	req.Header.Set("Referer", "https://blocked.example.com/path")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("referer-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Your referer host is not allowed"}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestBypassMissingAllowsMissingReferer(t *testing.T) {
	bypassMissing := true
	p := newTestPlugin(t, Config{
		BypassMissing: &bypassMissing,
		Whitelist:     []string{"allowed.example.com"},
	})

	called := false
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/restricted", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestLeadingStarMatchesHostSuffix(t *testing.T) {
	p := newTestPlugin(t, Config{Whitelist: []string{"*.example.com"}})
	req := httptest.NewRequest(http.MethodGet, "/restricted", nil)
	req.Header.Set("Referer", "https://api.example.com/path")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestNonLeadingStarIsLiteral(t *testing.T) {
	p := newTestPlugin(t, Config{Whitelist: []string{"example.*"}})
	req := httptest.NewRequest(http.MethodGet, "/restricted", nil)
	req.Header.Set("Referer", "https://example.com/path")

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("referer-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
