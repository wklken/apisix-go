package cors

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

func TestHandlerAllowsRegexOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:        "https://example.com",
		AllowMethods:        http.MethodGet,
		AllowOriginsByRegex: []string{`^https://.+\.test\.com$`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.test.com")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://api.test.com" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want regex origin", got)
	}
	if got := rr.Code; got != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", got, http.StatusNoContent)
	}
}

func TestHandlerAllowsDefaultWildcardMethods(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want wildcard", got)
	}
	if got := rr.Code; got != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", got, http.StatusNoContent)
	}
}

func TestHandlerReflectsDoubleStarRequestHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowHeaders: "**",
		AllowMethods: http.MethodGet,
	})
	req := httptest.NewRequest(http.MethodOptions, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Headers", "X-Foo, X-Bar")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("preflight should not reach upstream handler")
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Headers"); got != "X-Foo, X-Bar" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want reflected request headers", got)
	}
	if got := rr.Code; got != http.StatusOK {
		t.Fatalf("response code = %d, want %d", got, http.StatusOK)
	}
}

func TestHandlerDoubleStarOriginEchoesRequestOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins: "**",
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want request origin", got)
	}
}

func TestHandlerSetsTimingAllowOriginFromConfiguredOrigins(t *testing.T) {
	p := newTestPlugin(t, Config{
		TimingAllowOrigins: stringPtr("https://client.example,https://other.example"),
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Timing-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("Timing-Allow-Origin = %q, want matched request origin", got)
	}
}

func TestHandlerSetsTimingAllowOriginFromRegex(t *testing.T) {
	p := newTestPlugin(t, Config{
		TimingAllowOrigins:        stringPtr("https://client.example"),
		TimingAllowOriginsByRegex: []string{`^https://.+\.timing\.example$`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.timing.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Timing-Allow-Origin"); got != "https://api.timing.example" {
		t.Fatalf("Timing-Allow-Origin = %q, want regex-matched request origin", got)
	}
}

func TestHandlerSkipsTimingAllowOriginWhenNotMatched(t *testing.T) {
	p := newTestPlugin(t, Config{
		TimingAllowOrigins: stringPtr("https://client.example"),
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://blocked.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Timing-Allow-Origin"); got != "" {
		t.Fatalf("Timing-Allow-Origin = %q, want empty", got)
	}
}

func stringPtr(v string) *string {
	return &v
}
