package saml_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestUnauthenticatedRequestRedirectsToIDP(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, cfg.IDPURI+"?") {
		t.Fatalf("Location = %q, want IDP redirect", location)
	}
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if redirectURL.Query().Get("SAMLRequest") == "" {
		t.Fatalf("Location = %q, want SAMLRequest", location)
	}
	relayState := redirectURL.Query().Get("RelayState")
	if relayState == "" {
		t.Fatalf("Location = %q, want RelayState", location)
	}
	if got := findSetCookie(rr.Result().Cookies(), requestCookieName(p.sessionFingerprint(), relayState)); got == nil {
		t.Fatal("SAML request state cookie was not set")
	} else if got.Secure {
		t.Fatal("SAML request state cookie Secure = true, want false for HTTP-Redirect test")
	}
}

func TestHTTPPostBindingReturnsAutoSubmitForm(t *testing.T) {
	cfg := testConfig(t)
	cfg.AuthProtocolBindingMethod = "HTTP-POST"
	p := newTestPlugin(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `method="post"`) || !strings.Contains(body, `name="SAMLRequest"`) {
		t.Fatalf("body = %q, want SAML POST form", body)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
}

func TestExistingSessionPassesRequestAndSetsUserInfoHeader(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))
	cookie, err := p.sessionCookie(externalUser{
		NameID:     "alice@example.com",
		Attributes: map[string][]string{"role": {"admin"}},
	})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Userinfo"); got == "" {
			t.Fatal("X-Userinfo header was not set")
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
}

func TestLogoutDeletesSessionAndRedirectsToIDP(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	cookie, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/logout", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	if location := rr.Header().Get("Location"); !strings.Contains(location, "SAMLRequest=") {
		t.Fatalf("Location = %q, want SAML logout request", location)
	}
	deleted := findSetCookie(rr.Result().Cookies(), sessionCookieName(p.sessionFingerprint()))
	if deleted == nil || deleted.MaxAge != -1 {
		t.Fatalf("session delete cookie = %#v, want MaxAge=-1", deleted)
	}
}

func TestInvalidSAMLResponseIsRejected(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/login/callback",
		strings.NewReader("SAMLResponse=bad"),
	)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()

	certPEM, keyPEM := testCertificate(t)
	return Config{
		SPIssuer:                  "https://sp.example.com",
		IDPURI:                    "https://idp.example.com/sso",
		IDPCert:                   certPEM,
		LoginCallbackURI:          "http://example.com/login/callback",
		LogoutURI:                 "/logout",
		LogoutCallbackURI:         "http://example.com/logout/callback",
		LogoutRedirectURI:         "/logged-out",
		SPCert:                    certPEM,
		SPPrivateKey:              keyPEM,
		AuthProtocolBindingMethod: "HTTP-Redirect",
		Secret:                    strings.Repeat("s", 16),
	}
}

func testCertificate(t *testing.T) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(certPEM), string(keyPEM)
}

func findSetCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
