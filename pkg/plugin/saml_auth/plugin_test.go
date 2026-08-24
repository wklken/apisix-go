package saml_auth

import (
	"context"
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
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewjam/saml"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type samlScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type samlScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	calls  []samlScopedSecretCall
}

func (*samlScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*samlScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *samlScopedSecretBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, samlScopedSecretCall{Scope: scope, Raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private SAML test value")
	}
	return value, nil
}

func (*samlScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *samlScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func (broker *samlScopedSecretBroker) scopedCalls() []samlScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]samlScopedSecretCall(nil), broker.calls...)
}

func newSAMLScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *samlScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(
		revision, []generation.Resource{{Key: key, Value: document}}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "saml-auth-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &samlScopedSecretBroker{values: values}
	registration, err := secret.NewScopedMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		_ = registration.Close(context.Background())
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close SAML scoped attempt: %v", err)
		}
	}
}

func assertSAMLDescriptor(t *testing.T, got, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if got != want {
		t.Fatalf("descriptor = %q, want %q", got, want)
	}
}

func TestMaterializeScopedSecretsOwnsSAMLPrivateAndSessionKeys(t *testing.T) {
	const (
		privateRaw  = "$ENV://SAML_SP_PRIVATE_KEY"
		sessionRaw  = "$secret://vault/saml/session"
		fallbackOne = "$ENV://SAML_SESSION_PREVIOUS"
		fallbackTwo = "$secret://vault/saml/oldest"
	)
	config := testConfig(t)
	validPrivateKey := config.SPPrivateKey
	config.SPPrivateKey = privateRaw
	config.Secret = sessionRaw
	config.SecretFallbacks = []string{fallbackOne, fallbackTwo}
	capabilityValue, scope, broker, closeAttempt := newSAMLScopedSecretHarness(
		t, 90, "saml-scoped", config, map[string]string{
			privateRaw: "not-a-private-key", sessionRaw: "session-current",
			fallbackOne: "short", fallbackTwo: "session-oldest",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || strings.Contains(err.Error(), privateRaw) || strings.Contains(err.Error(), "not-a-private-key") {
		t.Fatalf("invalid resolved PEM error = %v, want redacted failure", err)
	}
	if p.spKeyPair != nil || p.secretsPrepared || p.config.SPPrivateKey != privateRaw ||
		p.config.Secret != sessionRaw || len(p.config.SecretFallbacks) != 2 {
		t.Fatalf("invalid PEM installed partial state: %#v", p)
	}

	broker.setValue(privateRaw, validPrivateKey)
	err = base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || strings.Contains(err.Error(), fallbackOne) || strings.Contains(err.Error(), "short") {
		t.Fatalf("invalid third value error = %v, want redacted failure", err)
	}
	if p.spKeyPair != nil || p.secretsPrepared || p.config.SPPrivateKey != privateRaw {
		t.Fatal("fallback failure installed parsed keypair or public descriptor")
	}

	broker.setValue(fallbackOne, "session-previous")
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	assertSAMLDescriptor(t, p.config.SPPrivateKey, validPrivateKey)
	assertSAMLDescriptor(t, p.config.Secret, "session-current")
	assertSAMLDescriptor(t, p.config.SecretFallbacks[0], "session-previous")
	assertSAMLDescriptor(t, p.config.SecretFallbacks[1], "session-oldest")
	if p.spKeyPair == nil || p.spKeyPair.key == nil || p.spKeyPair.cert == nil {
		t.Fatal("successful materialization did not install derived SP keypair")
	}

	calls := broker.scopedCalls()
	wantFields := []string{
		"sp_private_key",
		"sp_private_key", "secret", "secret_fallbacks",
		"sp_private_key", "secret", "secret_fallbacks", "secret_fallbacks",
	}
	wantRaw := []string{
		privateRaw,
		privateRaw, sessionRaw, fallbackOne,
		privateRaw, sessionRaw, fallbackOne, fallbackTwo,
	}
	if len(calls) != len(wantFields) {
		t.Fatalf("resolver calls = %#v, want fields %v", calls, wantFields)
	}
	for index, field := range wantFields {
		wantScope := scope
		wantScope.Field = field
		if calls[index].Scope != wantScope || calls[index].Raw != wantRaw[index] {
			t.Fatalf("resolver call %d = %#v, want scope %#v raw %q", index, calls[index], wantScope, wantRaw[index])
		}
	}

	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() with descriptor config error = %v", err)
	}
	t.Cleanup(p.Stop)
	sp, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if err != nil || sp.Key == nil || sp.Certificate == nil || sp.IDPMetadata == nil {
		t.Fatalf("serviceProvider() after materialization = %#v/%v", sp, err)
	}
}

func TestMaterializeScopedSecretsConcurrentCallsInstallOnce(t *testing.T) {
	config := testConfig(t)
	privateKey := config.SPPrivateKey
	config.SPPrivateKey = "$ENV://SAML_CONCURRENT_PRIVATE_KEY"
	config.Secret = "$ENV://SAML_CONCURRENT_SESSION"
	config.SecretFallbacks = []string{"$ENV://SAML_CONCURRENT_FALLBACK"}
	capabilityValue, scope, broker, closeAttempt := newSAMLScopedSecretHarness(
		t, 91, "saml-concurrent", config, map[string]string{
			config.SPPrivateKey:       privateKey,
			config.Secret:             "session-current",
			config.SecretFallbacks[0]: "session-previous",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialization error = %v", err)
		}
	}
	if calls := broker.scopedCalls(); len(calls) != 3 {
		t.Fatalf("resolver calls = %d, want one call per declared value", len(calls))
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
}

func TestSAMLResolvedSessionSecretRuneBoundaries(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		wantErr bool
	}{
		{name: "seven rejected", count: 7, wantErr: true},
		{name: "eight accepted", count: 8},
		{name: "thirty two accepted", count: 32},
		{name: "thirty three rejected", count: 33, wantErr: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			privateKey := config.SPPrivateKey
			config.SPPrivateKey = "$ENV://SAML_BOUNDARY_PRIVATE_KEY"
			config.Secret = "$ENV://SAML_BOUNDARY_SESSION"
			resolved := strings.Repeat("界", test.count)
			capabilityValue, scope, _, closeAttempt := newSAMLScopedSecretHarness(
				t, uint64(100+index), "saml-boundary", config, map[string]string{
					config.SPPrivateKey: privateKey,
					config.Secret:       resolved,
				},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
			if test.wantErr {
				if err == nil || p.secretsPrepared || p.config.Secret != config.Secret {
					t.Fatalf("resolved rune count %d result = %v, config=%#v", test.count, err, p.config)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertSAMLDescriptor(t, p.config.Secret, resolved)
			p.Stop()
		})
	}
}

func TestSAMLRawCookieTextIsValidatedAfterResolution(t *testing.T) {
	config := testConfig(t)
	config.Secret = "short"
	config.SecretFallbacks = []string{"tiny"}
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	rawConfig := map[string]any{
		"sp_issuer": config.SPIssuer, "idp_uri": config.IDPURI,
		"idp_cert": config.IDPCert, "login_callback_uri": config.LoginCallbackURI,
		"logout_uri": config.LogoutURI, "logout_callback_uri": config.LogoutCallbackURI,
		"logout_redirect_uri": config.LogoutRedirectURI, "sp_cert": config.SPCert,
		"sp_private_key": config.SPPrivateKey, "secret": config.Secret,
		"secret_fallbacks": []any{"tiny"},
	}
	if err := util.Validate(rawConfig, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected raw secret text before materialization: %v", err)
	}
	p.config = config
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("resolved short literal passed materialization")
	}
}

func TestSAMLLegacyMaterializationDecryptsContextualAndRotatedSecrets(t *testing.T) {
	const (
		currentKey = "0123456789abcdef"
		oldKey     = "fedcba9876543210"
		session    = "session-current"
		fallback   = "session-previous"
	)
	config := testConfig(t)
	privateKey := config.SPPrivateKey
	currentService := testutil.DataEncryptionService(true, []string{currentKey})
	oldService := testutil.DataEncryptionService(true, []string{oldKey})
	var err error
	config.SPPrivateKey, err = currentService.EncryptForContext(privateKey, name+".sp_private_key")
	if err != nil {
		t.Fatal(err)
	}
	config.Secret, err = currentService.EncryptForContext(session, name+".secret")
	if err != nil {
		t.Fatal(err)
	}
	oldFallback, err := oldService.EncryptForContext(fallback, name+".secret_fallbacks")
	if err != nil {
		t.Fatal(err)
	}
	config.SecretFallbacks = []string{oldFallback}
	p := &Plugin{config: config}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{currentKey, oldKey}).Resolver(),
	})
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	t.Cleanup(p.Stop)
	assertSAMLDescriptor(t, p.config.SPPrivateKey, privateKey)
	assertSAMLDescriptor(t, p.config.Secret, session)
	assertSAMLDescriptor(t, p.config.SecretFallbacks[0], fallback)
	if p.spIDPMetadata != nil {
		t.Fatal("materialization built IDP metadata before PostInit")
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

func TestRandomStateReturnsErrorForFailingReader(t *testing.T) {
	state, err := randomState(failingReader{})
	if err == nil {
		t.Fatal("randomState() error = nil, want failure")
	}
	if state != "" {
		t.Fatalf("randomState() = %q, want empty state on failure", state)
	}
	if !strings.Contains(err.Error(), "random unavailable") {
		t.Fatalf("randomState() error = %v, want failing reader wrapped", err)
	}
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
	if !apisixctx.IsSensitiveQueryName(req, "SAMLResponse") {
		t.Fatal("saml-auth did not register SAMLResponse query key")
	}

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

func TestMaterializeSecretsRejectsInvalidSPKeyPair(t *testing.T) {
	cfg := testConfig(t)
	cfg.SPPrivateKey = "not-a-valid-key"
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want invalid SP key pair rejection")
	}
}

func TestServiceProviderConcurrentRequestsShareParsedState(t *testing.T) {
	cfg := testConfig(t)
	p := newTestPlugin(t, cfg)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			sp, err := p.serviceProvider(httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
			if err != nil {
				t.Errorf("serviceProvider() error = %v", err)
				return
			}
			if sp.Key == nil || sp.Certificate == nil || sp.IDPMetadata == nil {
				t.Error("serviceProvider returned incomplete parsed state")
			}
		})
	}
	wg.Wait()
}

func TestSAMLGenerationsIsolateSignaturesAndSessionCookies(t *testing.T) {
	oldConfig := testConfig(t)
	oldConfig.Secret = "session-old-generation"
	newConfig := testConfig(t)
	newConfig.SPIssuer = oldConfig.SPIssuer
	newConfig.IDPURI = oldConfig.IDPURI
	newConfig.IDPEntityID = oldConfig.IDPEntityID
	newConfig.LoginCallbackURI = oldConfig.LoginCallbackURI
	newConfig.LogoutURI = oldConfig.LogoutURI
	newConfig.LogoutCallbackURI = oldConfig.LogoutCallbackURI
	newConfig.LogoutRedirectURI = oldConfig.LogoutRedirectURI
	newConfig.Secret = "session-new-generation"

	oldPlugin := newTestPlugin(t, oldConfig)
	newPlugin := newTestPlugin(t, newConfig)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	oldRecorder := httptest.NewRecorder()
	oldPlugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("old generation called next without a session")
	})).ServeHTTP(oldRecorder, request)
	oldRedirect, err := url.Parse(oldRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySAMLRedirectSignature(oldRedirect.RawQuery, "SAMLRequest", oldConfig.SPCert); err != nil {
		t.Fatalf("old signature with old certificate error = %v", err)
	}
	if err := verifySAMLRedirectSignature(oldRedirect.RawQuery, "SAMLRequest", newConfig.SPCert); err == nil {
		t.Fatal("old signature verified with new generation certificate")
	}

	oldCookie, err := oldPlugin.sessionCookie(externalUser{NameID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	newRequest := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	newRequest.AddCookie(oldCookie)
	if _, ok := newPlugin.sessionUser(newRequest); ok {
		t.Fatal("new generation accepted old cookie without explicit fallback")
	}

	rotatedConfig := newConfig
	rotatedConfig.SecretFallbacks = []string{oldConfig.Secret}
	rotated := newTestPlugin(t, rotatedConfig)
	rotatedRequest := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rotatedRequest.AddCookie(oldCookie)
	if user, ok := rotated.sessionUser(rotatedRequest); !ok || user.NameID != "alice" {
		t.Fatalf("explicit fallback user = %#v/%v, want alice/true", user, ok)
	}

	oldPlugin.Stop()
	if _, err := newPlugin.serviceProvider(
		httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil),
	); err != nil {
		t.Fatalf("stopping old generation retired new generation: %v", err)
	}
}

func TestSAMLSessionFallbackOrderMatchesConfiguration(t *testing.T) {
	config := testConfig(t)
	config.Secret = "session-current"
	config.SecretFallbacks = []string{"session-previous", "session-oldest"}
	p := newTestPlugin(t, config)
	if err := p.useSessionSecretsLocked(func(current string, fallbacks []string) error {
		if current != "session-current" ||
			fmt.Sprint(fallbacks) != "[session-previous session-oldest]" {
			t.Fatalf("session secrets = %q/%v, want configured order", current, fallbacks)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStopDrainsActiveHandlerAndPreventsResurrection(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))
	cookie, err := p.sessionCookie(externalUser{NameID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	request.AddCookie(cookie)
	go func() {
		defer close(handlerDone)
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(entered)
			<-release
		})).ServeHTTP(httptest.NewRecorder(), request)
	}()
	<-entered

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active handler drained")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-handlerDone
	<-stopDone
	p.Stop()
	if p.spKeyPair != nil || p.spIDPMetadata != nil || p.secretsPrepared ||
		p.legacySPPrivateKey != nil || p.legacySessionSecret != nil {
		t.Fatalf("Stop left private runtime state: %#v", p)
	}
	late := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired generation called next")
	})).ServeHTTP(late, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if late.Code != http.StatusServiceUnavailable {
		t.Fatalf("late handler status = %d, want 503", late.Code)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("MaterializeSecrets after Stop = %v", err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit after Stop = %v", err)
	}
}

func TestServiceProviderConcurrentReadsAndStop(t *testing.T) {
	p := newTestPlugin(t, testConfig(t))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 32 {
				sp, err := p.serviceProvider(
					httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil),
				)
				if err != nil {
					if !errors.Is(err, secret.ErrCredentialUnavailable) {
						t.Errorf("serviceProvider after retirement error = %v", err)
					}
					return
				}
				if sp.Key == nil || sp.Certificate == nil || sp.IDPMetadata == nil {
					t.Error("serviceProvider returned partial state")
					return
				}
			}
		})
	}
	close(start)
	p.Stop()
	wg.Wait()
}
