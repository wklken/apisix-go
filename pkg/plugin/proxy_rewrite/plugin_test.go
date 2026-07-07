package proxy_rewrite

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

func TestHandlerDerivesURIFromRegexURI(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexURI: []string{`^/users/(\d+)/profile$`, `/profiles/$1`},
	})
	var rewrite map[string]interface{}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value("proxy-rewrite").(map[string]interface{})
	}))

	req := httptest.NewRequest(http.MethodGet, "/users/42/profile", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/profiles/42" {
		t.Fatalf("rewrite uri = %q, want /profiles/42", got)
	}
}

func TestHandlerUsesFirstMatchingRegexURIPair(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexURI: []string{`^/orders/(\d+)$`, `/primary/$1`, `^/orders/(\d+)$`, `/fallback/$1`},
	})
	var rewrite map[string]interface{}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value("proxy-rewrite").(map[string]interface{})
	}))

	req := httptest.NewRequest(http.MethodGet, "/orders/7", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/primary/7" {
		t.Fatalf("rewrite uri = %q, want first replacement /primary/7", got)
	}
}

func TestHandlerURIHasPriorityOverRegexURI(t *testing.T) {
	p := newTestPlugin(t, Config{
		Uri:      "/static",
		RegexURI: []string{`^/users/(\d+)$`, `/profiles/$1`},
	})
	var rewrite map[string]interface{}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value("proxy-rewrite").(map[string]interface{})
	}))

	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/static" {
		t.Fatalf("rewrite uri = %q, want /static", got)
	}
}

func TestPostInitRejectsOddRegexURI(t *testing.T) {
	p := &Plugin{config: Config{RegexURI: []string{`^/users/(\d+)$`}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want odd regex_uri error")
	}
}
