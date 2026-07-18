package cors

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

var errHijackCalled = errors.New("hijack called")

type optionalInterfaceWriter struct {
	*httptest.ResponseRecorder
	flushed  bool
	hijacked bool
}

func (w *optionalInterfaceWriter) Flush() {
	w.flushed = true
}

func (w *optionalInterfaceWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, errHijackCalled
}

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

func TestPostInitAllowsCredentialsWithEmptyOptions(t *testing.T) {
	p := &Plugin{config: Config{
		AllowOrigins:    "http://test.com",
		AllowMethods:    "",
		AllowHeaders:    "",
		ExposeHeaders:   "",
		MaxAge:          600,
		AllowCredential: true,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want explicit empty options accepted", err)
	}
}

func TestHandlerRegexOriginsRestrictDefaultWildcard(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:        "*",
		AllowOriginsByRegex: []string{`^https://.+\.domain\.com$`},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://blocked.example.com")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want regex-mismatched origin rejected", got)
	}
}

func TestHandlerAddsAPISIXHeadersForAllowedOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:  "https://api.example",
		AllowMethods:  "GET,POST",
		AllowHeaders:  "request-h",
		ExposeHeaders: "expose-h",
		MaxAge:        10,
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":   "https://api.example",
		"Access-Control-Allow-Methods":  "GET,POST",
		"Access-Control-Allow-Headers":  "request-h",
		"Access-Control-Expose-Headers": "expose-h",
		"Access-Control-Max-Age":        "10",
	} {
		if got := rr.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestHandlerExpandsDoubleStarMethods(t *testing.T) {
	p := newTestPlugin(t, Config{AllowOrigins: "**", AllowMethods: "**"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	const want = "GET,POST,PUT,DELETE,PATCH,HEAD,OPTIONS,CONNECT,TRACE"
	if got := rr.Header().Get("Access-Control-Allow-Methods"); got != want {
		t.Fatalf("Access-Control-Allow-Methods = %q, want %q", got, want)
	}
}

func TestHandlerInterceptsOptionsWithoutOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{AllowOrigins: "**"})
	req := httptest.NewRequest(http.MethodOptions, "http://example.com/options", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("OPTIONS without Origin must not reach the upstream handler")
	})).ServeHTTP(rr, req)

	if got := rr.Code; got != http.StatusOK {
		t.Fatalf("response code = %d, want %d", got, http.StatusOK)
	}
	if got := rr.Body.String(); got != "" {
		t.Fatalf("response body = %q, want empty", got)
	}
}

func TestHandlerAppendsOriginToUpstreamVary(t *testing.T) {
	p := newTestPlugin(t, Config{AllowOrigins: "https://api.example"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://api.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Via")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Vary"); got != "Via, Origin" {
		t.Fatalf("Vary = %q, want %q", got, "Via, Origin")
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

func TestHandlerAllowsPrivateNetworkPreflight(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:        "https://client.example",
		AllowMethods:        http.MethodGet,
		AllowPrivateNetwork: true,
	})
	req := httptest.NewRequest(http.MethodOptions, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("private-network preflight should not reach upstream handler")
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Fatalf("Access-Control-Allow-Private-Network = %q, want true", got)
	}
	if got := rr.Code; got != http.StatusOK {
		t.Fatalf("response code = %d, want %d", got, http.StatusOK)
	}
}

func TestHandlerPreservesOptionalResponseWriterInterfaces(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	writer := &optionalInterfaceWriter{ResponseRecorder: httptest.NewRecorder()}

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped response writer does not implement http.Flusher")
		}
		flusher.Flush()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("wrapped response writer does not implement http.Hijacker")
		}
		if _, _, err := hijacker.Hijack(); !errors.Is(err, errHijackCalled) {
			t.Fatalf("Hijack() error = %v, want delegated sentinel", err)
		}
	})).ServeHTTP(writer, req)

	if !writer.flushed || !writer.hijacked {
		t.Fatalf("underlying capabilities called = flush %t, hijack %t", writer.flushed, writer.hijacked)
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

func TestHandlerAllowsOriginFromMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOriginsByMetadata: []string{"tenant_a"},
	})
	p.metadata.AllowOrigins = map[string]string{
		"tenant_a": "https://app.example,https://admin.example",
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://admin.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want metadata origin", got)
	}
}

func TestHandlerRestrictsDefaultWildcardWhenMetadataConfigured(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOriginsByMetadata: []string{"tenant_a"},
	})
	p.metadata.AllowOrigins = map[string]string{
		"tenant_a": "https://app.example",
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://blocked.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestHandlerFallsBackToConfiguredOriginAfterMetadataMiss(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowOrigins:           "https://fallback.example",
		AllowOriginsByMetadata: []string{"tenant_a"},
	})
	p.metadata.AllowOrigins = map[string]string{
		"tenant_a": "https://app.example",
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://fallback.example")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://fallback.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want configured fallback origin", got)
	}
}

func TestHandlerSetsTimingAllowOriginFromConfiguredOrigins(t *testing.T) {
	p := newTestPlugin(t, Config{
		TimingAllowOrigins: new("https://client.example,https://other.example"),
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
		TimingAllowOrigins:        new("https://client.example"),
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
		TimingAllowOrigins: new("https://client.example"),
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

func TestPostInitRejectsCredentialsWithWildcardOptions(t *testing.T) {
	p := &Plugin{config: Config{AllowCredential: true}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want wildcard credential rejection")
	}
}

func TestHandlerDoesNotExposeHeadersByDefault(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Origin", "https://client.example")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Internal", "value")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Expose-Headers"); got != "" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want empty by default", got)
	}
}
