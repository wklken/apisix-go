package proxy_rewrite

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	var rewrite map[string]any

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
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
	var rewrite map[string]any

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
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
	var rewrite map[string]any

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
	}))

	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/static" {
		t.Fatalf("rewrite uri = %q, want /static", got)
	}
}

func TestHandlerUsesRealRequestURIUnsafeAsRewriteSource(t *testing.T) {
	p := newTestPlugin(t, Config{
		UseRealRequestURIUnsafe: true,
	})
	var rewrite map[string]any

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
	}))

	req := httptest.NewRequest(http.MethodGet, "/files/%2Fraw?download=1", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/files/%2Fraw?download=1" {
		t.Fatalf("rewrite uri = %q, want real request URI", got)
	}
}

func TestHandlerRegexURIMatchesRealRequestURIUnsafe(t *testing.T) {
	p := newTestPlugin(t, Config{
		UseRealRequestURIUnsafe: true,
		RegexURI:                []string{`^/api/(.*)\?token=.*$`, `/private/$1?token=redacted`},
	})
	var rewrite map[string]any

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite = r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1?token=abc", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := rewrite["uri"].(string); got != "/private/v1?token=redacted" {
		t.Fatalf("rewrite uri = %q, want regex rewrite from real request URI", got)
	}
}

func TestHandlerMutatesRequestHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: Headers{
			Add: map[string]string{
				"X-Trace": "$request_method:$arg_id",
			},
			Set: map[string]string{
				"X-User": "$http_x_original_user",
			},
			Remove: []string{"X-Remove"},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Values("X-Trace"); len(got) != 1 || got[0] != "GET:42" {
			t.Fatalf("X-Trace values = %v, want [GET:42]", got)
		}
		if got := r.Header.Get("X-User"); got != "alice" {
			t.Fatalf("X-User = %q, want alice", got)
		}
		if got := r.Header.Get("X-Remove"); got != "" {
			t.Fatalf("X-Remove = %q, want empty", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/users?id=42", nil)
	req.Header.Set("X-Original-User", "alice")
	req.Header.Set("X-Remove", "gone")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestHandlerResolvesRegexURICapturesInHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexURI: []string{`^/users/(\d+)/orders/(\d+)$`, `/orders/$2/users/$1`},
		Headers: Headers{
			Set: map[string]string{
				"X-User-ID":  "$1",
				"X-Order-ID": "${2}",
				"X-Mixed":    "$request_method:$1:${2}",
			},
			Add: map[string]string{
				"X-Capture": "$1-${2}",
			},
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User-ID"); got != "42" {
			t.Fatalf("X-User-ID = %q, want 42", got)
		}
		if got := r.Header.Get("X-Order-ID"); got != "99" {
			t.Fatalf("X-Order-ID = %q, want 99", got)
		}
		if got := r.Header.Get("X-Mixed"); got != "GET:42:99" {
			t.Fatalf("X-Mixed = %q, want GET:42:99", got)
		}
		if got := r.Header.Values("X-Capture"); len(got) != 1 || got[0] != "42-99" {
			t.Fatalf("X-Capture values = %v, want [42-99]", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/users/42/orders/99", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestHeadersUnmarshalLegacySet(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"headers":{"X-Legacy":"$uri","X-Number":7}}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	p := newTestPlugin(t, cfg)
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Legacy"); got != "/legacy" {
			t.Fatalf("X-Legacy = %q, want /legacy", got)
		}
		if got := r.Header.Get("X-Number"); got != "7" {
			t.Fatalf("X-Number = %q, want 7", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/legacy", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
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
