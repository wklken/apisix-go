package cas_auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const testLogoutTrustedCIDR = "192.0.2.0/24"

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
	if !apisixctx.IsSensitiveQueryName(callbackReq, "ticket") {
		t.Fatal("cas-auth did not register ticket query key")
	}

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
		IDPURI:                 "https://cas.example.com",
		CASCallbackURI:         "/cas_callback",
		LogoutURI:              "/logout",
		LogoutTrustedAddresses: []string{testLogoutTrustedCIDR},
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

func TestIdPLogoutRequestAcceptsXMLDeclaration(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:                 "https://cas.example.com",
		CASCallbackURI:         "/cas_callback",
		LogoutURI:              "/logout",
		LogoutTrustedAddresses: []string{testLogoutTrustedCIDR},
		Cookie:                 CookieConfig{Secret: strings.Repeat("s", 32), Secure: new(false)},
	})
	p.storeSession("ST-xml", "alice")
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/cas_callback",
		strings.NewReader(
			`<?xml version="1.0" encoding="UTF-8"?><samlp:LogoutRequest><samlp:SessionIndex>ST-xml</samlp:SessionIndex></samlp:LogoutRequest>`,
		),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
		ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if testSessionExists(p, "ST-xml") {
		t.Fatal("valid XML declaration request did not delete the CAS session")
	}
}

func TestIdPLogoutRequestRequiresOneDirectNonEmptySessionIndex(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<samlp:Other><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:Other>`},
		{name: "missing", body: `<samlp:LogoutRequest></samlp:LogoutRequest>`},
		{
			name: "empty",
			body: `<samlp:LogoutRequest><samlp:SessionIndex>   </samlp:SessionIndex></samlp:LogoutRequest>`,
		},
		{
			name: "duplicate",
			body: `<samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex><samlp:SessionIndex>ST-2</samlp:SessionIndex></samlp:LogoutRequest>`,
		},
		{
			name: "nested",
			body: `<samlp:LogoutRequest><samlp:Issuer><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:Issuer></samlp:LogoutRequest>`,
		},
		{
			name: "trailing data",
			body: `<samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:LogoutRequest>x`,
		},
		{
			name: "directive",
			body: `<!DOCTYPE LogoutRequest><samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:LogoutRequest>`,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				IDPURI:                 "https://cas.example.com",
				CASCallbackURI:         "/cas_callback",
				LogoutURI:              "/logout",
				LogoutTrustedAddresses: []string{testLogoutTrustedCIDR},
				Cookie:                 CookieConfig{Secret: strings.Repeat("s", 32), Secure: new(false)},
			})
			p.storeSession("ST-1", "alice")
			req := httptest.NewRequest(http.MethodPost, "http://example.com/cas_callback", strings.NewReader(test.body))
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
				ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
			if !testSessionExists(p, "ST-1") {
				t.Fatal("invalid logout request deleted the CAS session")
			}
		})
	}
}

func TestIdPLogoutRequestRejectsOversizedBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:                 "https://cas.example.com",
		CASCallbackURI:         "/cas_callback",
		LogoutURI:              "/logout",
		LogoutTrustedAddresses: []string{testLogoutTrustedCIDR},
		Cookie:                 CookieConfig{Secret: strings.Repeat("s", 32), Secure: new(false)},
	})
	p.storeSession("ST-1", "alice")
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/cas_callback",
		strings.NewReader(strings.Repeat("x", 64*1024+1)),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
		ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !testSessionExists(p, "ST-1") {
		t.Fatal("oversized logout request deleted the CAS session")
	}
}

func TestIdPLogoutRequestChecksConfiguredSocketPeerCIDR(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:                 "https://cas.example.com",
		CASCallbackURI:         "/cas_callback",
		LogoutURI:              "/logout",
		LogoutTrustedAddresses: []string{"192.0.2.0/24"},
		Cookie:                 CookieConfig{Secret: strings.Repeat("s", 32), Secure: new(false)},
	})
	body := `<samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:LogoutRequest>`

	p.storeSession("ST-1", "alice")
	untrusted := httptest.NewRequest(http.MethodPost, "http://example.com/cas_callback", strings.NewReader(body))
	untrusted.RemoteAddr = "198.51.100.10:1234"
	untrusted.Header.Set("X-Forwarded-For", "192.0.2.10")
	untrustedRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
		ServeHTTP(untrustedRR, untrusted)
	if untrustedRR.Code != http.StatusForbidden {
		t.Fatalf("untrusted status = %d, want 403", untrustedRR.Code)
	}
	if !testSessionExists(p, "ST-1") {
		t.Fatal("untrusted logout request deleted the CAS session")
	}

	trusted := httptest.NewRequest(http.MethodPost, "http://example.com/cas_callback", strings.NewReader(body))
	trusted.RemoteAddr = "192.0.2.10:1234"
	trustedRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
		ServeHTTP(trustedRR, trusted)
	if trustedRR.Code != http.StatusOK {
		t.Fatalf("trusted status = %d, want 200", trustedRR.Code)
	}
	if testSessionExists(p, "ST-1") {
		t.Fatal("trusted logout request did not delete the CAS session")
	}
}

func TestIdPLogoutRequestRejectsEmptyTrustedAddressList(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie:         CookieConfig{Secret: strings.Repeat("s", 32), Secure: new(false)},
	})
	p.storeSession("ST-1", "alice")
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/cas_callback",
		strings.NewReader(`<samlp:LogoutRequest><samlp:SessionIndex>ST-1</samlp:SessionIndex></samlp:LogoutRequest>`),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next handler called") })).
		ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 when trusted CIDRs are omitted", rr.Code)
	}
	if !testSessionExists(p, "ST-1") {
		t.Fatal("SLO without trusted CIDRs deleted the CAS session")
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
	processSessions.put(foreign.sessionKey("ST-forged"), sessionEntry{
		fingerprint: issuer.sessionOptions().fingerprint,
		user:        "alice",
	})
	if foreign.refreshSession("ST-forged") {
		t.Fatal("a plugin instance accepted a stored entry with another config fingerprint")
	}
	issuer.deleteSession("ST-shared")
}

func TestSessionStoreExpiresEntries(t *testing.T) {
	store, err := newSessionStore(2, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("newSessionStore() error = %v", err)
	}
	store.put("session", sessionEntry{fingerprint: "fp", user: "alice"})
	time.Sleep(30 * time.Millisecond)
	if store.refresh("session", "fp") {
		t.Fatal("expired session was refreshed")
	}
}

func TestSessionStoreRefreshesTTL(t *testing.T) {
	store, err := newSessionStore(2, 60*time.Millisecond)
	if err != nil {
		t.Fatalf("newSessionStore() error = %v", err)
	}
	store.put("session", sessionEntry{fingerprint: "fp", user: "alice"})
	time.Sleep(40 * time.Millisecond)
	if !store.refresh("session", "fp") {
		t.Fatal("live session was not refreshed")
	}
	time.Sleep(40 * time.Millisecond)
	if !store.refresh("session", "fp") {
		t.Fatal("refresh did not extend the session TTL")
	}
}

func TestSessionStoreEvictsLeastRecentlyUsedAtCapacity(t *testing.T) {
	store, err := newSessionStore(2, time.Hour)
	if err != nil {
		t.Fatalf("newSessionStore() error = %v", err)
	}
	store.put("old", sessionEntry{fingerprint: "fp", user: "old"})
	store.put("recent", sessionEntry{fingerprint: "fp", user: "recent"})
	if !store.refresh("old", "fp") {
		t.Fatal("old session was not present before recency refresh")
	}
	store.put("new", sessionEntry{fingerprint: "fp", user: "new"})
	if store.refresh("recent", "fp") {
		t.Fatal("least recently used session survived capacity eviction")
	}
	if !store.refresh("old", "fp") || !store.refresh("new", "fp") {
		t.Fatal("recent sessions were evicted instead of the least recently used entry")
	}
	if got := store.cache.Len(); got != 2 {
		t.Fatalf("store length = %d, want bounded capacity 2", got)
	}
}

func TestSessionStoreConcurrentRefreshAndMutation(t *testing.T) {
	store, err := newSessionStore(32, time.Minute)
	if err != nil {
		t.Fatalf("newSessionStore() error = %v", err)
	}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for iteration := range 500 {
				key := fmt.Sprintf("%d-%d", worker, iteration%16)
				store.put(key, sessionEntry{fingerprint: "fp", user: "alice"})
				_ = store.refresh(key, "fp")
				if iteration%3 == 0 {
					store.remove(key)
				}
			}
		})
	}
	wg.Wait()
	if got := store.cache.Len(); got > 32 {
		t.Fatalf("store length = %d, exceeds capacity 32", got)
	}
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

func TestPostInitRejectsSameSiteNoneWithoutSecureCookie(t *testing.T) {
	secureFalse := false
	p := &Plugin{config: Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret:   strings.Repeat("s", 32),
			SameSite: "None",
			Secure:   &secureFalse,
		},
	}}
	err := p.PostInit()
	if err == nil {
		t.Fatal("SameSite=None with secure=false passed PostInit validation")
	}
	want := `cookie.secure must be true when cookie.samesite is "None"`
	if err.Error() != want {
		t.Fatalf("PostInit error = %q, want %q", err.Error(), want)
	}

	secureTrue := true
	p2 := &Plugin{config: Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret:   strings.Repeat("s", 32),
			SameSite: "None",
			Secure:   &secureTrue,
		},
	}}
	if err := p2.PostInit(); err != nil {
		t.Fatalf("SameSite=None with secure=true failed PostInit validation: %v", err)
	}
}

func TestPostInitRejectsInvalidLogoutTrustedAddress(t *testing.T) {
	p := &Plugin{config: Config{
		IDPURI:                 "https://cas.example.com",
		CASCallbackURI:         "/cas_callback",
		LogoutURI:              "/logout",
		LogoutTrustedAddresses: []string{"not-a-cidr"},
		Cookie:                 CookieConfig{Secret: strings.Repeat("s", 32)},
	}}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() accepted invalid logout_trusted_addresses CIDR")
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
	signed := base.SignRawSessionValue([]byte("/foo?bar=baz"), p.config.Cookie.Secret)
	decoded, ok := base.VerifyRawSessionValue(signed, p.config.Cookie.Secret, nil)
	if !ok || string(decoded) != "/foo?bar=baz" {
		t.Fatalf("raw session roundtrip = %q, %t; want /foo?bar=baz, true", decoded, ok)
	}

	tampered := signed[:len(signed)-1] + "A"
	if tampered == signed {
		tampered = signed[:len(signed)-1] + "B"
	}
	if _, ok := base.VerifyRawSessionValue(tampered, p.config.Cookie.Secret, nil); ok {
		t.Fatal("VerifyRawSessionValue(tampered) = true, want false")
	}
	if _, ok := base.VerifyRawSessionValue(signed, strings.Repeat("X", 32), nil); ok {
		t.Fatal("VerifyRawSessionValue(wrong secret) = true, want false")
	}
	for _, malformed := range []string{"", "no-dot-here", "abc.def"} {
		if _, ok := base.VerifyRawSessionValue(malformed, p.config.Cookie.Secret, nil); ok {
			t.Fatalf("VerifyRawSessionValue(%q) = true, want false", malformed)
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
		if got := base.CallbackPath(callback); got != want {
			t.Errorf("base.CallbackPath(%q) = %q, want %q", callback, got, want)
		}
	}
}

func withLocalAddress(r *http.Request, address net.Addr) context.Context {
	return context.WithValue(r.Context(), http.LocalAddrContextKey, address)
}

func testSessionExists(p *Plugin, sessionID string) bool {
	_, ok := processSessions.cache.Peek(p.sessionKey(sessionID))
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
