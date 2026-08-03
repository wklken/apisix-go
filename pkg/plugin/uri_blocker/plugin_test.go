package uri_blocker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockedURIDefaultResponseHasNoBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules: []string{`^/blocked`},
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/blocked?a=1", nil))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := rr.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty", got)
	}
}

func TestBlockedURICustomMessageUsesErrorMessageJSON(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules:   []string{`^/blocked`},
		RejectedMsg:  "blocked by uri",
		RejectedCode: http.StatusTeapot,
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/blocked", nil))

	if rr.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTeapot)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"error_msg":"blocked by uri"}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestCaseInsensitiveMatch(t *testing.T) {
	caseInsensitive := true
	p := newTestPlugin(t, Config{
		BlockRules:      []string{`^/blocked`},
		CaseInsensitive: &caseInsensitive,
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/BLOCKED", nil))

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestPostInitRejectsInvalidRegularExpression(t *testing.T) {
	p := &Plugin{config: Config{BlockRules: []string{`.+(`}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid regular expression rejected")
	}
}

func TestNormalizedPathCannotBypassAnchoredRule(t *testing.T) {
	p := newTestPlugin(t, Config{BlockRules: []string{`^/internal/`}})
	req := httptest.NewRequest(http.MethodGet, "/./internal/x?aa=1", nil)
	req.URL.Path = "/internal/x"

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("uri-blocker should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestAllowedURIFallsThrough(t *testing.T) {
	p := newTestPlugin(t, Config{
		BlockRules: []string{`^/blocked`},
	})

	called := false
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/allowed", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNoContent)
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
