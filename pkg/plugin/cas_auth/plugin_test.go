package cas_auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
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

func TestUnauthenticatedRequestRedirectsToCASLogin(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, "https://cas.example.com/login?") {
		t.Fatalf("Location = %q, want CAS login URL", location)
	}
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if redirectURL.Query().Get("service") != "http://example.com:80/cas_callback" {
		t.Fatalf("service = %q, want callback URL", redirectURL.Query().Get("service"))
	}
	if got := findSetCookie(rr.Result().Cookies(), "CAS_REQUEST_URI"); got == nil {
		t.Fatal("CAS_REQUEST_URI cookie was not set")
	} else if got.Secure {
		t.Fatal("CAS_REQUEST_URI Secure = true, want false from config")
	} else if got.MaxAge != 0 || !got.Expires.IsZero() {
		t.Fatalf("CAS_REQUEST_URI persistence = MaxAge %d Expires %v, want session-only", got.MaxAge, got.Expires)
	}
}

func TestCallbackValidatesTicketAndCreatesSession(t *testing.T) {
	var validateQuery url.Values
	casServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/serviceValidate" {
			t.Fatalf("CAS path = %q, want /serviceValidate", r.URL.Path)
		}
		validateQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(
			[]byte(
				`<cas:serviceResponse><cas:authenticationSuccess><cas:user>alice</cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
			),
		)
	}))
	t.Cleanup(casServer.Close)

	p := newTestPlugin(t, Config{
		IDPURI:         casServer.URL,
		CASCallbackURI: "http://example.com/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	})

	initReq := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	initRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(initRR, initReq)
	stateCookie := findSetCookie(initRR.Result().Cookies(), "CAS_REQUEST_URI")
	if stateCookie == nil {
		t.Fatal("CAS_REQUEST_URI cookie was not set")
	}

	callbackReq := httptest.NewRequest(http.MethodGet, "http://example.com/cas_callback?ticket=ST-1", nil)
	callbackReq.AddCookie(stateCookie)
	callbackRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for callback")
	})).ServeHTTP(callbackRR, callbackReq)

	if callbackRR.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", callbackRR.Code)
	}
	if callbackRR.Header().Get("Location") != "/orders/1" {
		t.Fatalf("Location = %q, want original URI", callbackRR.Header().Get("Location"))
	}
	if validateQuery.Get("ticket") != "ST-1" {
		t.Fatalf("validated ticket = %q, want ST-1", validateQuery.Get("ticket"))
	}
	if validateQuery.Get("service") != "http://example.com/cas_callback" {
		t.Fatalf("validated service = %q, want callback URL", validateQuery.Get("service"))
	}
	if got := findSessionCookie(callbackRR.Result().Cookies()); got == nil {
		t.Fatal("CAS session cookie was not set")
	} else if got.Value != "ST-1" {
		t.Fatalf("session cookie value = %q, want ticket", got.Value)
	} else if got.MaxAge != 0 || !got.Expires.IsZero() {
		t.Fatalf("session cookie persistence = MaxAge %d Expires %v, want session-only", got.MaxAge, got.Expires)
	}
	if got := findSetCookie(callbackRR.Result().Cookies(), "CAS_REQUEST_URI"); got == nil || got.MaxAge != -1 {
		t.Fatalf("CAS_REQUEST_URI delete cookie = %#v, want MaxAge=-1", got)
	}
}

func TestExistingSessionPassesRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	})
	sessionName := p.sessionOptions().cookieName
	p.storeSession("ST-1", "alice")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders/1", nil)
	req.AddCookie(&http.Cookie{Name: sessionName, Value: "ST-1"})
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

func TestIdPLogoutRequestDeletesMatchingCASSession(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	})
	p.storeSession("ST-1", "alice")

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/cas_callback",
		strings.NewReader(`<samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:LogoutRequest>`),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler was called for a valid SLO callback")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if testSessionExists(p, "ST-1") {
		t.Fatal("CAS session still exists after IdP logout request")
	}
}

func TestLogoutDeletesSessionAndRedirectsToCASLogout(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	})
	sessionName := p.sessionOptions().cookieName
	p.storeSession("ST-1", "alice")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionName, Value: "ST-1"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	if rr.Header().Get("Location") != "https://cas.example.com/logout" {
		t.Fatalf("Location = %q, want CAS logout URL", rr.Header().Get("Location"))
	}
	deleted := findSetCookie(rr.Result().Cookies(), sessionName)
	if deleted == nil || deleted.MaxAge != -1 {
		t.Fatalf("session delete cookie = %#v, want MaxAge=-1", deleted)
	}
	if testSessionExists(p, "ST-1") {
		t.Fatal("session still exists after logout")
	}
}

func TestSessionsAreSharedAcrossPluginInstancesAndNamespacedByConfig(t *testing.T) {
	cfg := Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: new(false),
		},
	}
	issuer := newTestPlugin(t, cfg)
	reloaded := newTestPlugin(t, cfg)
	foreignConfig := cfg
	foreignConfig.CASCallbackURI = "/other_callback"
	foreign := newTestPlugin(t, foreignConfig)

	issuer.storeSession("ST-shared", "alice")
	if !reloaded.refreshSession("ST-shared") {
		t.Fatal("a plugin instance for the same config did not observe the process-local session")
	}
	if foreign.refreshSession("ST-shared") {
		t.Fatal("a plugin instance for another config observed the foreign session")
	}
	processSessions.Lock()
	processSessions.entries[foreign.sessionKey("ST-forged")] = sessionEntry{
		fingerprint: issuer.sessionOptions().fingerprint,
		user:        "alice",
		expiresAt:   time.Now().Add(time.Minute),
	}
	processSessions.Unlock()
	if foreign.refreshSession("ST-forged") {
		t.Fatal("a plugin instance accepted a stored entry with another config fingerprint")
	}
	issuer.deleteSession("ST-shared")
}

func TestRelativeServiceURLUsesListenerPortNotForgedHostPort(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie:         CookieConfig{Secret: strings.Repeat("s", 32)},
	})
	req := httptest.NewRequest(http.MethodGet, "http://internal.example/orders", nil)
	req.Host = "attacker.example.net:9443"
	req = req.WithContext(withLocalAddress(req, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1984}))

	if got := p.serviceURL(req); got != "http://attacker.example.net:1984/cas_callback" {
		t.Fatalf("serviceURL() = %q, want listener port with request host", got)
	}
}

func TestSchemaRejectsSameSiteNoneWithoutSecureCookie(t *testing.T) {
	p := newTestPlugin(t, Config{})
	config := map[string]any{
		"idp_uri":          "https://cas.example.com",
		"cas_callback_uri": "/cas_callback",
		"logout_uri":       "/logout",
		"cookie": map[string]any{
			"secret":   strings.Repeat("s", 32),
			"samesite": "None",
			"secure":   false,
		},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("SameSite=None with secure=false passed schema validation")
	}
	config["cookie"].(map[string]any)["secure"] = true
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("SameSite=None with secure=true failed schema validation: %v", err)
	}
}

func TestSafeRedirectMatrix(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{name: "path", uri: "/foo", want: true},
		{name: "path with query", uri: "/foo?bar=baz", want: true},
		{name: "external URL", uri: "https://evil.example/x", want: false},
		{name: "protocol relative URL", uri: "//evil.example/x", want: false},
		{name: "backslash authority", uri: `\\evil.example`, want: false},
		{name: "header injection", uri: "/foo\r\nLocation: x", want: false},
		{name: "empty", uri: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeRedirect(test.uri); got != test.want {
				t.Fatalf("safeRedirect(%q) = %v, want %v", test.uri, got, test.want)
			}
		})
	}
}

func TestSignedStateRoundTripAndTamperMatrix(t *testing.T) {
	p := newTestPlugin(t, Config{Cookie: CookieConfig{Secret: "0123456789abcdef0123456789abcdef"}})
	signed, err := p.signValue("/foo?bar=baz")
	if err != nil {
		t.Fatalf("signValue() error = %v", err)
	}
	if got := p.verifyValue(signed); got != "/foo?bar=baz" {
		t.Fatalf("verifyValue(roundtrip) = %q", got)
	}

	tampered := signed[:len(signed)-1] + "A"
	if tampered == signed {
		tampered = signed[:len(signed)-1] + "B"
	}
	if got := p.verifyValue(tampered); got != "" {
		t.Fatalf("verifyValue(tampered) = %q, want empty", got)
	}
	wrongSecret := newTestPlugin(t, Config{Cookie: CookieConfig{Secret: strings.Repeat("X", 32)}})
	if got := wrongSecret.verifyValue(signed); got != "" {
		t.Fatalf("verifyValue(wrong secret) = %q, want empty", got)
	}
	for _, malformed := range []string{"", "no-dot-here", "abc.def"} {
		if got := p.verifyValue(malformed); got != "" {
			t.Fatalf("verifyValue(%q) = %q, want empty", malformed, got)
		}
	}
}

func TestCallbackPathMatrix(t *testing.T) {
	tests := map[string]string{
		"/cas_callback":                        "/cas_callback",
		"https://app.example.com/cas_callback": "/cas_callback",
		"http://app.example.com:8443/cb":       "/cb",
		"https://app.example.com":              "/",
		"https://app.example.com/cb?from=cas":  "/cb",
		"https://app.example.com/cb#fragment":  "/cb",
	}
	for callback, want := range tests {
		if got := callbackPath(callback); got != want {
			t.Errorf("callbackPath(%q) = %q, want %q", callback, got, want)
		}
	}
}

func withLocalAddress(r *http.Request, address net.Addr) context.Context {
	return context.WithValue(r.Context(), http.LocalAddrContextKey, address)
}

func testSessionExists(p *Plugin, sessionID string) bool {
	processSessions.Lock()
	defer processSessions.Unlock()
	_, ok := processSessions.entries[p.sessionKey(sessionID)]
	return ok
}

func findSetCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func findSessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if strings.HasPrefix(cookie.Name, "CAS_SESSION_") {
			return cookie
		}
	}
	return nil
}
