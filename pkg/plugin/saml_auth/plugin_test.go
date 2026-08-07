package saml_auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewjam/saml"
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

func TestSessionFromDifferentIDPEntityIsRejected(t *testing.T) {
	firstConfig := testConfig(t)
	firstConfig.IDPEntityID = "https://idp.example.com/realms/first"
	first := newTestPlugin(t, firstConfig)
	cookie, err := first.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	secondConfig := firstConfig
	secondConfig.IDPEntityID = "https://idp.example.com/realms/second"
	second := newTestPlugin(t, secondConfig)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	second.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("session from the previous IdP entity reached the upstream")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want authentication redirect 302", response.Code)
	}
}

func TestOmittedIDPEntityKeepsLegacySessionFingerprint(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	legacy := sha256.Sum256([]byte(cfg.SPIssuer + "|" + cfg.IDPURI + "|" + cfg.LoginCallbackURI))
	want := hex.EncodeToString(legacy[:])[:16]

	if got := p.sessionFingerprint(); got != want {
		t.Fatalf("session fingerprint = %q, want legacy value %q", got, want)
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
	redirectURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse logout redirect: %v", err)
	}
	if redirectURL.Query().Get("SigAlg") != rsaSHA256Method ||
		redirectURL.Query().Get("Signature") == "" {
		t.Fatalf("Location = %q, want external Redirect binding signature", redirectURL)
	}
	rawRequest, err := decodeSAMLRedirectValue(redirectURL.Query().Get("SAMLRequest"))
	if err != nil {
		t.Fatalf("decode logout request: %v", err)
	}
	var logoutRequest saml.LogoutRequest
	if err := xml.Unmarshal(rawRequest, &logoutRequest); err != nil {
		t.Fatalf("unmarshal logout request: %v", err)
	}
	if logoutRequest.Signature != nil {
		t.Fatal("Redirect LogoutRequest contains an enveloped XML signature")
	}
	relayState := redirectURL.Query().Get("RelayState")
	if relayState == "" {
		t.Fatal("logout redirect did not contain RelayState")
	}
	if stateCookie := findSetCookie(
		rr.Result().Cookies(),
		logoutCookieName(p.sessionFingerprint(), relayState),
	); stateCookie == nil {
		t.Fatal("logout redirect did not set correlated state cookie")
	}
	deleted := findSetCookie(rr.Result().Cookies(), sessionCookieName(p.sessionFingerprint()))
	if deleted == nil || deleted.MaxAge != -1 {
		t.Fatalf("session delete cookie = %#v, want MaxAge=-1", deleted)
	}
}

func TestUnsignedLogoutCallbackIsRejectedWithoutClearingSession(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))
	session, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/logout/callback", nil)
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if deleted := findSetCookie(rr.Result().Cookies(), sessionCookieName(p.sessionFingerprint())); deleted != nil {
		t.Fatalf("unsigned callback deleted session: %#v", deleted)
	}
}

func TestSignedLogoutRequestClearsSessionAndReturnsCorrelatedResponse(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	session, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	idp := testSAMLSigner(t, cfg.IDPURI, cfg.SPIssuer, cfg.IDPCert, cfg.SPPrivateKey)
	logoutRequest, redirect, err := signedRedirectLogoutRequest(
		idp,
		cfg.LogoutCallbackURI,
		"alice@example.com",
		"idp-relay",
	)
	if err != nil {
		t.Fatalf("signedRedirectLogoutRequest() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, redirect.String(), nil)
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	response, err := decodeLogoutResponse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("decode logout response: %v", err)
	}
	if response.InResponseTo != logoutRequest.ID {
		t.Fatalf("InResponseTo = %q, want %q", response.InResponseTo, logoutRequest.ID)
	}
	if response.Destination != cfg.IDPURI {
		t.Fatalf("Destination = %q, want %q", response.Destination, cfg.IDPURI)
	}
	if deleted := findSetCookie(rr.Result().Cookies(), sessionCookieName(p.sessionFingerprint())); deleted == nil {
		t.Fatal("valid IdP logout request did not clear session")
	}
}

func TestTamperedLogoutRequestIsRejectedWithoutClearingSession(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	session, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	idp := testSAMLSigner(t, cfg.IDPURI, cfg.SPIssuer, cfg.IDPCert, cfg.SPPrivateKey)
	_, redirect, err := signedRedirectLogoutRequest(
		idp,
		cfg.LogoutCallbackURI,
		"alice@example.com",
		"idp-relay",
	)
	if err != nil {
		t.Fatalf("signedRedirectLogoutRequest() error = %v", err)
	}
	query := redirect.Query()
	query.Set("RelayState", "tampered-relay")
	redirect.RawQuery = query.Encode()

	req := httptest.NewRequest(http.MethodGet, redirect.String(), nil)
	req.AddCookie(session)
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if deleted := findSetCookie(rr.Result().Cookies(), sessionCookieName(p.sessionFingerprint())); deleted != nil {
		t.Fatalf("tampered logout request deleted session: %#v", deleted)
	}
}

func TestLogoutResponseRequiresStoredRequestCorrelation(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	session, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	start := httptest.NewRequest(http.MethodGet, "http://example.com/logout", nil)
	start.AddCookie(session)
	startRecorder := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(startRecorder, start)
	redirectURL, err := url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse logout redirect: %v", err)
	}
	relayState := redirectURL.Query().Get("RelayState")
	stateCookie := findSetCookie(
		startRecorder.Result().Cookies(),
		logoutCookieName(p.sessionFingerprint(), relayState),
	)
	if stateCookie == nil {
		t.Fatal("logout state cookie was not set")
	}

	idp := testSAMLSigner(t, cfg.IDPURI, cfg.SPIssuer, cfg.IDPCert, cfg.SPPrivateKey)
	_, wrongRedirect, err := signedRedirectLogoutResponse(
		idp,
		cfg.LogoutCallbackURI,
		"wrong-request-id",
		relayState,
	)
	if err != nil {
		t.Fatalf("signedRedirectLogoutResponse() error = %v", err)
	}
	callback := httptest.NewRequest(http.MethodGet, wrongRedirect.String(), nil)
	callback.AddCookie(stateCookie)
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(callbackRecorder, callback)

	if callbackRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", callbackRecorder.Code)
	}
}

func TestPostLogoutResponseUsesValidatedFormCorrelation(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)
	session, err := p.sessionCookie(externalUser{NameID: "alice@example.com"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	start := httptest.NewRequest(http.MethodGet, "http://example.com/logout", nil)
	start.AddCookie(session)
	startRecorder := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(startRecorder, start)
	redirectURL, err := url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse logout redirect: %v", err)
	}
	requestXML, err := decodeSAMLRedirectValue(redirectURL.Query().Get("SAMLRequest"))
	if err != nil {
		t.Fatalf("decode logout request: %v", err)
	}
	var request saml.LogoutRequest
	if err := xml.Unmarshal(requestXML, &request); err != nil {
		t.Fatalf("unmarshal logout request: %v", err)
	}
	relayState := redirectURL.Query().Get("RelayState")
	stateCookie := findSetCookie(
		startRecorder.Result().Cookies(),
		logoutCookieName(p.sessionFingerprint(), relayState),
	)
	if stateCookie == nil {
		t.Fatal("logout state cookie was not set")
	}

	idp := testSAMLSigner(t, cfg.IDPURI, cfg.SPIssuer, cfg.IDPCert, cfg.SPPrivateKey)
	response, err := idp.MakeLogoutResponse(cfg.LogoutCallbackURI, request.ID)
	if err != nil {
		t.Fatalf("MakeLogoutResponse() error = %v", err)
	}
	responseXML, err := samlElementBytes(response.Element())
	if err != nil {
		t.Fatalf("LogoutResponse.Bytes() error = %v", err)
	}
	form := url.Values{
		"SAMLResponse": {base64.StdEncoding.EncodeToString(responseXML)},
		"RelayState":   {relayState},
	}
	callback := httptest.NewRequest(
		http.MethodPost,
		cfg.LogoutCallbackURI,
		strings.NewReader(form.Encode()),
	)
	callback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	callback.AddCookie(stateCookie)
	recorder := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(recorder, callback)

	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Location"); got != cfg.LogoutRedirectURI {
		t.Fatalf("Location = %q, want %q", got, cfg.LogoutRedirectURI)
	}
}

func TestServiceProviderSeparatesIDPEntityFromEndpoint(t *testing.T) {
	cfg := testConfig(t)
	cfg.IDPEntityID = "https://idp.example.com/realms/integration"
	p := newTestPlugin(t, cfg)
	sp, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if err != nil {
		t.Fatalf("serviceProvider() error = %v", err)
	}
	if sp.IDPMetadata.EntityID != cfg.IDPEntityID {
		t.Fatalf("IdP entity ID = %q, want %q", sp.IDPMetadata.EntityID, cfg.IDPEntityID)
	}
	if got := sp.GetSSOBindingLocation(saml.HTTPRedirectBinding); got != cfg.IDPURI {
		t.Fatalf("SSO endpoint = %q, want %q", got, cfg.IDPURI)
	}
}

func TestIDPEntityIDDefaultsToEndpointForCompatibility(t *testing.T) {
	cfg := testConfig(t)
	if got := cfg.idpEntityID(); got != cfg.IDPURI {
		t.Fatalf("idpEntityID() = %q, want compatibility endpoint %q", got, cfg.IDPURI)
	}
}

func TestSignedRedirectPreservesEndpointQueryAndCanonicalSignature(t *testing.T) {
	cfg := testConfig(t)
	signer := testSAMLSigner(t, cfg.SPIssuer, cfg.idpEntityID(), cfg.SPCert, cfg.SPPrivateKey)
	redirect, err := signedSAMLRedirectURL(
		cfg.IDPURI+"?tenant=a",
		"SAMLRequest",
		[]byte(`<samlp:LogoutRequest xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol"/>`),
		"original",
		signer.Key,
	)
	if err != nil {
		t.Fatalf("signedSAMLRedirectURL() error = %v", err)
	}
	if got := redirect.Query().Get("tenant"); got != "a" {
		t.Fatalf("tenant query = %q, want preserved value a", got)
	}
	if err := verifySAMLRedirectSignature(redirect.RawQuery, "SAMLRequest", cfg.SPCert); err != nil {
		t.Fatalf("verify redirect signature: %v", err)
	}
	tampered := strings.Replace(redirect.RawQuery, "RelayState=original", "RelayState=tampered", 1)
	if err := verifySAMLRedirectSignature(tampered, "SAMLRequest", cfg.SPCert); err == nil {
		t.Fatal("tampered RelayState passed Redirect signature verification")
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

func TestCallbackParserFailureReturnsAuthenticationFailure(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler should not be called")
	})

	start := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	startRecorder := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(startRecorder, start)
	redirectURL, err := url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	relayState := redirectURL.Query().Get("RelayState")
	if relayState == "" {
		t.Fatal("authentication redirect did not contain RelayState")
	}
	stateCookie := findSetCookie(
		startRecorder.Result().Cookies(),
		requestCookieName(p.sessionFingerprint(), relayState),
	)
	if stateCookie == nil {
		t.Fatal("authentication redirect did not set state cookie")
	}

	callback := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/login/callback",
		strings.NewReader("SAMLResponse=bad&RelayState="+url.QueryEscape(relayState)),
	)
	callback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	callback.AddCookie(stateCookie)
	recorder := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(recorder, callback)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "saml authentication failed") {
		t.Fatalf("body = %q, want authentication failure", body)
	}
}

func TestSAMLAuthenticationDiagnosticExcludesResponseMaterial(t *testing.T) {
	diagnostic := errors.New("signature verification failed")
	err := &saml.InvalidResponseError{
		PrivateErr: diagnostic,
		Response:   `<Response><Assertion>secret</Assertion></Response>`,
	}
	if got := samlAuthenticationDiagnostic(err); !errors.Is(got, diagnostic) {
		t.Fatalf("diagnostic = %v, want private parser error", got)
	}
	if strings.Contains(samlAuthenticationDiagnostic(err).Error(), "secret") {
		t.Fatal("diagnostic leaked SAML response material")
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

func testSAMLSigner(
	t *testing.T,
	entityID string,
	peerEntityID string,
	certPEM string,
	keyPEM string,
) *saml.ServiceProvider {
	t.Helper()

	cert, key, err := parseKeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parseKeyPair() error = %v", err)
	}
	return &saml.ServiceProvider{
		EntityID:        entityID,
		Key:             key,
		Certificate:     cert,
		SignatureMethod: rsaSHA256Method,
		IDPMetadata:     &saml.EntityDescriptor{EntityID: peerEntityID},
	}
}

func findSetCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestServiceProviderReusesParsedKeyPairForRepeatedRequests(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)

	first, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if err != nil {
		t.Fatalf("serviceProvider(first) error = %v", err)
	}
	second, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if err != nil {
		t.Fatalf("serviceProvider(second) error = %v", err)
	}
	if first.Certificate != second.Certificate {
		t.Fatal("serviceProvider re-parsed the SP certificate per request")
	}
	if first.Key != second.Key {
		t.Fatal("serviceProvider re-parsed the SP private key per request")
	}
	if first.IDPMetadata != second.IDPMetadata {
		t.Fatal("serviceProvider rebuilt the IdP metadata per request")
	}
}

func TestPostInitRejectsInvalidSPKeyPair(t *testing.T) {
	cfg := testConfig(t)
	cfg.SPPrivateKey = "not-a-valid-key"
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid SP key pair rejection")
	}
}

func TestServiceProviderConcurrentRequestsShareParsedState(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sp, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
			if err != nil {
				t.Errorf("serviceProvider() error = %v", err)
				return
			}
			if sp.Key == nil || sp.Certificate == nil || sp.IDPMetadata == nil {
				t.Error("serviceProvider returned incomplete parsed state")
			}
		}()
	}
	wg.Wait()
}
