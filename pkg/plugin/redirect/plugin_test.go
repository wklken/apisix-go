package redirect

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitRequiresEffectiveConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "effective config is required" {
		t.Fatalf("PostInit() error = %v, want stable missing-config error", err)
	}
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

func TestSchemaRejectsRetCodeAboveHTTPMaximum(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":      "/new",
		"ret_code": 1000,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("ret_code=1000 should fail schema validation")
	}
}

func TestHandlerRegexURIReplacesFirstMatchOnly(t *testing.T) {
	p := newTestPlugin(t, Config{
		RegexUri: []string{`foo`, `bar`},
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/foo/foo", nil))
	if got := res.Header().Get("Location"); got != "/bar/foo" {
		t.Fatalf("Location = %q, want first-match /bar/foo", got)
	}
}

func TestHandlerAppendsRequestQueryToRegexURI(t *testing.T) {
	appendQueryString := true
	p := newTestPlugin(t, Config{
		RegexUri:          []string{`^/test/(.*)$`, `http://test.com/$1?q=apisix`},
		AppendQueryString: &appendQueryString,
	})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/test/hello?o=apache", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "http://test.com/hello?q=apisix&o=apache" {
		t.Fatalf("Location = %q, want regex redirect URI with the request query appended", got)
	}
}

func TestHandlerDoesNotAppendEmptyRequestQuery(t *testing.T) {
	appendQueryString := true
	p := newTestPlugin(t, Config{Uri: "/new", AppendQueryString: &appendQueryString})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/old", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "/new" {
		t.Fatalf("Location = %q, want no trailing question mark", got)
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

	if got := res.Header().Get("Location"); got != "/new%20path/%E4%B8%AD%E6%96%87?keep=1" {
		t.Fatalf("Location = %q, want encoded redirect URI", got)
	}
}

func TestHandlerUsesRelativeLocationForRelativeURI(t *testing.T) {
	p := newTestPlugin(t, Config{Uri: "/new"})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/old", nil)
	req.Host = "example.com"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "/new" {
		t.Fatalf("Location = %q, want /new", got)
	}
}

func TestHandlerExpandsRedirectVariables(t *testing.T) {
	p := newTestPlugin(t, Config{Uri: "$uri/to/${arg_name}/$bad_var"})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/from?name=json", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != "/from/to/json/" {
		t.Fatalf("Location = %q, want /from/to/json/", got)
	}
}

func TestHandlerPreservesRedirectDollarEscapes(t *testing.T) {
	p := newTestPlugin(t, Config{Uri: `/foo$$uri/\$uri/$uri`})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "/from", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if got := res.Header().Get("Location"); got != `/foo$/from/\$uri//from` {
		t.Fatalf("Location = %q, want dollar escapes preserved", got)
	}
}

func TestPostInitRejectsIncompatibleRedirectOptions(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "http_to_https and uri",
			config: Config{
				HttpToHttps: new(true),
				Uri:         "/foo",
			},
		},
		{
			name: "http_to_https and append_query_string",
			config: Config{
				HttpToHttps:       new(true),
				AppendQueryString: new(true),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: test.config}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want incompatible redirect options rejected")
			}
		})
	}
}

func TestPostInitUsesConfiguredSSLListenPort(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{
		Apisix: config.Apisix{
			Ssl: config.Ssl{
				Enable: true,
				Listen: []config.Listen{{Port: 9443}},
			},
		},
	}}
	httpToHTTPS := true
	p := &Plugin{config: Config{HttpToHttps: &httpToHTTPS}}
	p.SetDependencies(base.Dependencies{Config: effective})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.httpsPort == nil || *p.config.httpsPort != 9443 {
		t.Fatalf("https port = %v, want 9443 from apisix.ssl.listen", p.config.httpsPort)
	}
}

func TestHandlerRedirectsHTTPToHTTPSWithQueryAndConfiguredPort(t *testing.T) {
	httpToHTTPS := true
	port := 9443
	p := newTestPlugin(t, Config{HttpToHttps: &httpToHTTPS})
	p.config.httpsPort = &port
	handler := p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com:9080/orders?state=open", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusMovedPermanently {
		t.Fatalf("response code = %d, want 301", res.Code)
	}
	if got := res.Header().Get("Location"); got != "https://example.com:9443/orders?state=open" {
		t.Fatalf("Location = %q, want HTTPS URL with configured port and query", got)
	}
}

func TestHandlerTrustsForwardedHTTPSAndFallsThrough(t *testing.T) {
	httpToHTTPS := true
	p := newTestPlugin(t, Config{HttpToHttps: &httpToHTTPS})
	called := false
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if !called || res.Code != http.StatusNoContent {
		t.Fatalf("forwarded HTTPS called=%v status=%d, want fallthrough 204", called, res.Code)
	}
}

func TestHandlerDoesNotConsumeRequestBodyWithoutBodyDerivedOptions(t *testing.T) {
	// regex_uri that does not match falls through to the next handler, which
	// must still see the original request body untouched.
	p := newTestPlugin(t, Config{RegexUri: []string{`^/moved`, "/$1"}})

	var downstreamBody string
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("downstream read body: %v", err)
		}
		downstreamBody = string(body)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/original", strings.NewReader(`{"payload":"x"}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if downstreamBody != `{"payload":"x"}` {
		t.Fatalf("downstream body = %q, want the untouched request body", downstreamBody)
	}
}
