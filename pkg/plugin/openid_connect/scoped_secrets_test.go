package openid_connect

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"golang.org/x/oauth2"
)

type oidcScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type oidcScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []oidcScopedSecretCall
	hook   func(oidcScopedSecretCall)
}

func (*oidcScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*oidcScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *oidcScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	call := oidcScopedSecretCall{Scope: scope, Raw: raw}
	broker.mu.Lock()
	broker.calls = append(broker.calls, call)
	failure := broker.fail[raw]
	value, found := broker.values[raw]
	hook := broker.hook
	broker.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if failure != nil {
		return "", failure
	}
	if found {
		return value, nil
	}
	return raw, nil
}

func (broker *oidcScopedSecretBroker) setHook(hook func(oidcScopedSecretCall)) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.hook = hook
}

func (*oidcScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *oidcScopedSecretBroker) callsSnapshot() []oidcScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]oidcScopedSecretCall(nil), broker.calls...)
}

func (broker *oidcScopedSecretBroker) resetCalls() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = nil
}

func (broker *oidcScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func newOIDCScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *oidcScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte("{\"plugins\":{}}"),
	}}, nil)
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
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
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
	broker := &oidcScopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
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
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func oidcDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func materializeScopedOIDCSecrets(
	t *testing.T, p *Plugin, capabilityValue secret.GenerationCapability, scope secret.Scope,
) {
	t.Helper()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func assertOIDCScopedCalls(
	t *testing.T, scope secret.Scope, calls []oidcScopedSecretCall, fields, raws []string,
) {
	t.Helper()
	if len(calls) != len(fields) || len(fields) != len(raws) {
		t.Fatalf("broker calls = %#v, want fields=%#v raws=%#v", calls, fields, raws)
	}
	for index := range fields {
		wantScope := scope
		wantScope.Field = fields[index]
		if calls[index].Scope != wantScope || calls[index].Raw != raws[index] {
			t.Fatalf("call[%d] = %#v, want scope %#v raw %q", index, calls[index], wantScope, raws[index])
		}
	}
}

func oidcRSAFixtures(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))
	return privatePEM, publicKeyPEM(t, &privateKey.PublicKey)
}

func TestScopedSecretsMaterializeAllOIDCFields(t *testing.T) {
	privateKey, publicKey := oidcRSAFixtures(t)
	raws := []string{
		"$ENV://OIDC_CLIENT_SECRET", "$ENV://OIDC_PRIVATE_KEY", "$ENV://OIDC_PUBLIC_KEY",
		"$ENV://OIDC_SESSION_SECRET", "$ENV://OIDC_REDIS_PASSWORD",
	}
	values := map[string]string{
		raws[0]: "resolved-client-secret", raws[1]: privateKey, raws[2]: publicKey,
		raws[3]: "resolved-session-secret", raws[4]: "resolved-redis-password",
	}
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(t, 7, "all", values)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raws[0], Discovery: "https://idp.test/discovery",
		ClientRSAPrivateKey: raws[1], PublicKey: raws[2],
		Session: SessionConfig{Secret: raws[3], Storage: "redis", Redis: &SessionRedisConfig{
			Host: "redis.test", Password: raws[4],
		}},
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	fields := []string{
		"client_secret", "client_rsa_private_key", "public_key",
		"session.secret", "session.redis.password",
	}
	assertOIDCScopedCalls(t, scope, broker.callsSnapshot(), fields, raws)
	gotDescriptors := []string{
		p.config.ClientSecret, p.config.ClientRSAPrivateKey, p.config.PublicKey,
		p.config.Session.Secret, p.config.Session.Redis.Password,
	}
	for index, raw := range raws {
		if gotDescriptors[index] != oidcDescriptor(values[raw]) {
			t.Fatalf("descriptor[%d] = %q, want digest of resolved field", index, gotDescriptors[index])
		}
	}
	if p.clientRSAPrivateKey == nil || p.staticPublicKey == nil {
		t.Fatalf("derived RSA keys = private:%v public:%v, want both", p.clientRSAPrivateKey, p.staticPublicKey)
	}
	wantSessionKey := sha256.Sum256([]byte(values[raws[3]]))
	if got := p.sessionKey(); string(got) != string(wantSessionKey[:]) {
		t.Fatal("sessionKey() was not derived from resolved session secret")
	}
}

func TestScopedSecretsSkipAbsentOIDCFieldsForBearerOnly(t *testing.T) {
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(t, 8, "bearer", nil)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: "https://idp.test/discovery", BearerOnly: true, UseJWKS: true,
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if calls := broker.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("absent optional field broker calls = %#v, want none", calls)
	}
}

func TestScopedSecretsResolveManagedOIDCClientSecret(t *testing.T) {
	const (
		raw       = "$secret://vault/oidc/client-secret"
		plaintext = "managed-client-secret"
	)
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(
		t, 9, "managed", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raw, Discovery: "https://idp.test/discovery",
		BearerOnly: true,
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	assertOIDCScopedCalls(t, scope, broker.callsSnapshot(), []string{"client_secret"}, []string{raw})
	if p.config.ClientSecret != oidcDescriptor(plaintext) {
		t.Fatalf("client_secret descriptor = %q", p.config.ClientSecret)
	}
	if err := p.withClientSecret(func(value string) error {
		if value != plaintext {
			t.Fatalf("client secret = %q, want resolved value", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScopedSecretsRejectInvalidOIDCPublicKeyAtomically(t *testing.T) {
	privateKey, publicKey := oidcRSAFixtures(t)
	raws := []string{
		"$ENV://OIDC_ATOMIC_CLIENT",
		"$ENV://OIDC_ATOMIC_PRIVATE",
		"$ENV://OIDC_ATOMIC_PUBLIC",
	}
	values := map[string]string{raws[0]: "client-plaintext", raws[1]: privateKey, raws[2]: "invalid-public-key"}
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(t, 10, "atomic", values)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raws[0], Discovery: "https://idp.test/discovery",
		ClientRSAPrivateKey: raws[1], PublicKey: raws[2], BearerOnly: true,
	}}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("invalid public key error = %v, want redacted credential failure", err)
	}
	assertOIDCScopedCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"client_secret", "client_rsa_private_key", "public_key"}, raws,
	)
	if p.config.ClientSecret != raws[0] || p.config.ClientRSAPrivateKey != raws[1] ||
		p.config.PublicKey != raws[2] || p.clientRSAPrivateKey != nil || p.staticPublicKey != nil {
		t.Fatalf("failed materialization installed partial state: config=%#v", p.config)
	}
	if err := p.withClientSecret(func(string) error { return nil }); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("withClientSecret() error = %v, want unavailable before atomic install", err)
	}
	broker.setValue(raws[2], publicKey)
	broker.resetCalls()
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	assertOIDCScopedCalls(
		t, scope, broker.callsSnapshot(),
		[]string{"client_secret", "client_rsa_private_key", "public_key"}, raws,
	)
}

func TestScopedSecretsOIDCConfigAndErrorsAreRedacted(t *testing.T) {
	const (
		raw       = "$secret://vault/oidc/redacted-client"
		plaintext = "top-secret-oidc-value"
	)
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(
		t, 11, "redacted", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	broker.fail[raw] = fmt.Errorf("backend rejected %s %s", raw, plaintext)
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raw, Discovery: "https://idp.test/discovery", BearerOnly: true,
	}}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("materialization error = %v, want bounded redacted error", err)
	}
	for _, sensitive := range []string{raw, plaintext} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("materialization error leaked %q: %v", sensitive, err)
		}
	}
}

func TestScopedSecretsOIDCClientSecretAuthMethodsUseResolvedValue(t *testing.T) {
	const (
		raw       = "$ENV://OIDC_RUNTIME_CLIENT_SECRET"
		plaintext = "runtime-client-secret"
	)
	for index, method := range []string{"client_secret_basic", "client_secret_post", "client_secret_jwt"} {
		t.Run(method, func(t *testing.T) {
			requests := make(chan *http.Request, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm() error = %v", err)
				}
				requests <- r.Clone(r.Context())
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
				t, uint64(20+index), "auth-"+method, map[string]string{raw: plaintext},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				ClientID: "apisix", ClientSecret: raw, Discovery: server.URL,
				BearerOnly: true, TokenEndpointAuthMethod: method,
			}}
			materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			defer p.Stop()
			form := url.Values{}
			response, err := p.postTokenForm(
				httptest.NewRequest(http.MethodPost, "https://gateway.test", nil),
				server.URL,
				form,
			)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.Request.Header.Get("Authorization") != "" ||
				response.Request.Body != http.NoBody ||
				response.Request.GetBody != nil {
				t.Fatalf("returned HTTP response retained credential-bearing request: %#v", response.Request)
			}
			if form.Get("client_secret") != "" || form.Get("client_assertion") != "" {
				t.Fatalf("caller form retained credentials: %#v", form)
			}
			request := <-requests
			switch method {
			case "client_secret_basic":
				username, password, ok := request.BasicAuth()
				if !ok || username != "apisix" || password != plaintext {
					t.Fatalf("basic credentials = %q/%q/%v", username, password, ok)
				}
			case "client_secret_post":
				if request.PostForm.Get("client_secret") != plaintext {
					t.Fatalf("posted client_secret = %q", request.PostForm.Get("client_secret"))
				}
			case "client_secret_jwt":
				assertion, err := base.ParseJWT(request.PostForm.Get("client_assertion"))
				if err != nil {
					t.Fatal(err)
				}
				mac := hmac.New(sha256.New, []byte(plaintext))
				_, _ = mac.Write([]byte(assertion.Signing))
				if !hmac.Equal(assertion.Signature, mac.Sum(nil)) {
					t.Fatal("client_secret_jwt was not signed by the resolved secret")
				}
			}
		})
	}
}

func TestOIDCCodeExchangeAndRefreshClearRetainedClientCredentials(t *testing.T) {
	const (
		raw       = "$ENV://OIDC_TOKEN_GRANT_CLIENT_SECRET"
		plaintext = "token-grant-client-secret"
		endpoint  = "https://idp.test/token"
	)
	for index, method := range []string{"client_secret_basic", "client_secret_post"} {
		t.Run(method, func(t *testing.T) {
			capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
				t, uint64(120+index), "token-grant-"+method, map[string]string{raw: plaintext},
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				ClientID: "apisix", ClientSecret: raw,
				Discovery: endpoint, BearerOnly: true, TokenEndpointAuthMethod: method,
			}}
			materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
			if err := p.PostInit(); err != nil {
				t.Fatal(err)
			}
			defer p.Stop()
			p.mu.Lock()
			p.discovery = discoveryData{TokenEndpoint: endpoint}
			p.discoveryLoaded = true
			p.mu.Unlock()
			transport := &retainingOIDCTokenRoundTripper{}
			p.client = &http.Client{Transport: transport}
			request := httptest.NewRequest(http.MethodGet, "https://gateway.test/callback", nil)
			request = request.WithContext(context.WithValue(request.Context(), oauth2.HTTPClient, p.client))

			if _, err := p.exchangeCode(
				request, "code-a", "https://gateway.test/callback", "",
			); err != nil {
				t.Fatalf("exchangeCode() error = %v", err)
			}
			if _, err := p.refreshAccessToken(request, "refresh-a"); err != nil {
				t.Fatalf("refreshAccessToken() error = %v", err)
			}

			requests := transport.snapshot()
			if len(requests) != 2 {
				t.Fatalf("token requests = %d, want exchange and refresh", len(requests))
			}
			wantGrants := []string{"authorization_code", "refresh_token"}
			for requestIndex, retained := range requests {
				if retained.form.Get("grant_type") != wantGrants[requestIndex] {
					t.Fatalf(
						"request[%d] grant = %q, want %q",
						requestIndex,
						retained.form.Get("grant_type"),
						wantGrants[requestIndex],
					)
				}
				switch method {
				case "client_secret_basic":
					username, password, ok := parseOIDCBasicAuthorization(retained.authorization)
					if !ok || username != "apisix" || password != plaintext {
						t.Fatalf("request[%d] basic auth = %q/%q/%v", requestIndex, username, password, ok)
					}
				case "client_secret_post":
					if retained.form.Get("client_id") != "apisix" || retained.form.Get("client_secret") != plaintext {
						t.Fatalf("request[%d] post credentials = %#v", requestIndex, retained.form)
					}
				}
				if retained.request.Header.Get("Authorization") != "" ||
					retained.request.Body != http.NoBody || retained.request.GetBody != nil ||
					retained.request.ContentLength != 0 {
					t.Fatalf("request[%d] retained credential material after send: %#v", requestIndex, retained.request)
				}
			}
		})
	}
}

func TestOIDCHandlerRequiresSuccessfulPostInitPublication(t *testing.T) {
	config := codeFlowConfig("https://idp.test")
	config.UnauthAction = "pass"
	assertUnavailable := func(t *testing.T, p *Plugin) {
		t.Helper()
		p.mu.Lock()
		p.discovery = discoveryData{
			Issuer: "https://idp.test", AuthorizationEndpoint: "https://idp.test/authorize",
			TokenEndpoint: "https://idp.test/token",
		}
		p.discoveryLoaded = true
		p.mu.Unlock()
		if provider, err := p.providerClient(
			httptest.NewRequest(http.MethodGet, "https://gateway.test/orders", nil),
		); !errors.Is(err, secret.ErrCredentialUnavailable) || provider != nil {
			t.Fatalf("unready providerClient() = %#v/%v, want nil/credential unavailable", provider, err)
		}
		called := false
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://gateway.test/orders", nil))
		if called || recorder.Code != http.StatusInternalServerError {
			t.Fatalf("unready Handler called next/status = %v/%d, want false/500", called, recorder.Code)
		}
	}

	t.Run("before materialization", func(t *testing.T) {
		p := &Plugin{config: config}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		defer p.Stop()
		assertUnavailable(t, p)
	})
	t.Run("after materialization before PostInit", func(t *testing.T) {
		p := &Plugin{config: config}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		materializeOIDCTestPlugin(t, p)
		defer p.Stop()
		assertUnavailable(t, p)
	})
	t.Run("after failed PostInit", func(t *testing.T) {
		failedConfig := config
		failedConfig.TokenEndpointAuthMethod = "unsupported"
		p := &Plugin{config: failedConfig}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		materializeOIDCTestPlugin(t, p)
		defer p.Stop()
		if err := p.PostInit(); err == nil {
			t.Fatal("PostInit() error = nil, want unsupported auth method")
		}
		assertUnavailable(t, p)
	})
	t.Run("after successful PostInit", func(t *testing.T) {
		p := newTestPlugin(t, config)
		called := false
		recorder := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://gateway.test/orders", nil))
		if !called || recorder.Code != http.StatusOK {
			t.Fatalf("ready Handler called next/status = %v/%d, want true/200", called, recorder.Code)
		}
	})
}

func TestOIDCStopWinsBeforePostInitReadyPublication(t *testing.T) {
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: "publication-client-secret",
		Discovery: "https://idp.test/discovery", BearerOnly: true,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeOIDCTestPlugin(t, p)
	publicationEntered := make(chan struct{})
	publicationRelease := make(chan struct{})
	p.beforeReadyPublish = func() {
		close(publicationEntered)
		<-publicationRelease
	}
	postInitDone := make(chan error, 1)
	go func() { postInitDone <- p.PostInit() }()
	select {
	case <-publicationEntered:
	case <-time.After(time.Second):
		t.Fatal("PostInit did not reach the ready-publication barrier")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	deadline := time.Now().Add(time.Second)
	for !p.retired.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !p.retired.Load() {
		close(publicationRelease)
		<-postInitDone
		<-stopDone
		t.Fatal("Stop did not retire OIDC before publication resumed")
	}
	select {
	case <-stopDone:
		close(publicationRelease)
		<-postInitDone
		t.Fatal("Stop returned while PostInit was blocked before publication")
	default:
	}
	close(publicationRelease)
	if err := <-postInitDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit after retirement error = %v, want credential unavailable", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after PostInit left publication barrier")
	}
	if p.ready.Load() || p.client != nil || p.provider != nil || p.sessionStore != nil ||
		p.clientRelease != nil || p.scopedSet {
		t.Fatalf("retired PostInit left ready/runtime state: %#v", p)
	}
}

type retainedOIDCTokenRequest struct {
	request       *http.Request
	form          url.Values
	authorization string
}

type retainingOIDCTokenRoundTripper struct {
	mu       sync.Mutex
	requests []retainedOIDCTokenRequest
}

func (transport *retainingOIDCTokenRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	const tokenResponseBody = `{"access_token":"access-a","refresh_token":"refresh-b","expires_in":3600}`
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	transport.mu.Lock()
	transport.requests = append(transport.requests, retainedOIDCTokenRequest{
		request: request, form: form, authorization: request.Header.Get("Authorization"),
	})
	transport.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(tokenResponseBody)),
		Request:    request,
	}, nil
}

func (transport *retainingOIDCTokenRoundTripper) snapshot() []retainedOIDCTokenRequest {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]retainedOIDCTokenRequest(nil), transport.requests...)
}

func parseOIDCBasicAuthorization(value string) (string, string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(value, prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", "", false
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	return username, password, ok
}

func TestScopedSecretsOIDCPrivateKeyJWTUsesDerivedKey(t *testing.T) {
	privateKeyPEM, _ := oidcRSAFixtures(t)
	block, _ := pem.Decode([]byte(privateKeyPEM))
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "$ENV://OIDC_RUNTIME_PRIVATE_KEY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if !verifyClientAssertionSignature(r.PostForm.Get("client_assertion"), &privateKey.PublicKey) {
			t.Error("private_key_jwt did not use the derived private key")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
		t, 30, "private-key", map[string]string{raw: privateKeyPEM},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: server.URL, BearerOnly: true,
		ClientRSAPrivateKey: raw, TokenEndpointAuthMethod: "private_key_jwt",
		IntrospectionEndpointAuthMethod: "private_key_jwt",
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	response, err := p.postTokenForm(
		httptest.NewRequest(http.MethodPost, "https://gateway.test", nil),
		server.URL,
		url.Values{},
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

func TestScopedSecretsOIDCStaticVerificationUsesDerivedPublicKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const raw = "$ENV://OIDC_RUNTIME_PUBLIC_KEY"
	publicKey := publicKeyPEM(t, &privateKey.PublicKey)
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
		t, 31, "public-key", map[string]string{raw: publicKey},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: "https://idp.test/discovery", BearerOnly: true,
		PublicKey: raw,
		ClaimValidator: map[string]any{"issuer": map[string]any{
			"valid_issuers": []any{"https://issuer.test"},
		}},
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	token := signRS256(t, privateKey, map[string]any{
		"iss": "https://issuer.test", "aud": "apisix", "exp": timeNowUnix() + 3600,
	})
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test", nil)
	if _, err := p.verifyBearerJWT(request, token); err != nil {
		t.Fatalf("verifyBearerJWT() error = %v", err)
	}
}

func TestScopedSecretsOIDCSessionSealOpenUsesDerivedKey(t *testing.T) {
	const (
		raw       = "$ENV://OIDC_RUNTIME_SESSION_SECRET"
		plaintext = "resolved-session-secret"
	)
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
		t, 32, "session", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: "https://idp.test/discovery", UsePKCE: true,
		Session: SessionConfig{Secret: raw},
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	sealed, err := p.sealSession([]byte("session-payload"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := p.openSession(sealed)
	if err != nil || string(opened) != "session-payload" {
		t.Fatalf("openSession() = %q, %v", opened, err)
	}
	if strings.Contains(sealed, plaintext) || strings.Contains(sealed, raw) {
		t.Fatalf("sealed session leaked secret: %q", sealed)
	}
}

func TestScopedSecretsOIDCRedisAndProviderUseResolvedClientSecrets(t *testing.T) {
	const (
		clientRaw       = "$ENV://OIDC_PROVIDER_SECRET"
		clientPlaintext = "provider-client-secret"
		sessionRaw      = "$ENV://OIDC_PROVIDER_SESSION"
		sessionPlain    = "provider-session-secret"
		redisRaw        = "$ENV://OIDC_PROVIDER_REDIS"
		redisPlaintext  = "redis-password"
	)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fmt.Appendf(nil,
			"{\"issuer\":%q,\"authorization_endpoint\":%q,\"token_endpoint\":%q}",
			idpURL(r), idpURL(r)+"/authorize", idpURL(r)+"/token",
		))
	}))
	defer idp.Close()
	values := map[string]string{
		clientRaw: clientPlaintext, sessionRaw: sessionPlain, redisRaw: redisPlaintext,
	}
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(t, 33, "provider-redis", values)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: clientRaw, Discovery: idp.URL,
		Session: SessionConfig{Secret: sessionRaw, Storage: "redis", Redis: &SessionRedisConfig{
			Host: "127.0.0.1", Password: redisRaw,
		}},
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	client, err := p.providerClient(httptest.NewRequest(http.MethodGet, "https://gateway.test", nil))
	if err != nil {
		t.Fatal(err)
	}
	if client.oauth2Config.ClientSecret != clientPlaintext {
		t.Fatalf("provider client secret = %q", client.oauth2Config.ClientSecret)
	}
	redisStore, ok := p.sessionStore.(*redisSessionStore)
	if !ok {
		t.Fatalf("session store = %T, want Redis", p.sessionStore)
	}
	if got := redisStore.client.Options().Password; got != redisPlaintext {
		t.Fatalf("Redis option password = %q, want resolved password", got)
	}
	for _, sensitive := range []string{clientRaw, clientPlaintext, redisRaw, redisPlaintext} {
		if strings.Contains(fmt.Sprintf("%#v", p.config), sensitive) {
			t.Fatalf("public config retained %q: %#v", sensitive, p.config)
		}
	}
	p.Stop()
	if client.oauth2Config.ClientSecret != "" || p.provider != nil || p.sessionStore != nil {
		t.Fatal("Stop() retained provider/session runtime credentials")
	}
}

func idpURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestOIDCStopDrainsScopedClientSecretUseAndDropsState(t *testing.T) {
	const raw = "$ENV://OIDC_STOP_SCOPED_CLIENT"
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(
		t, 40, "stop-scoped", map[string]string{raw: "blocked-client-secret"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raw, Discovery: "https://idp.test", BearerOnly: true,
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	entered := make(chan struct{})
	release := make(chan struct{})
	useDone := make(chan error, 1)
	go func() {
		useDone <- p.withClientSecret(func(string) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(release)
		<-useDone
		t.Fatal("Stop() returned before scoped client-secret callback completed")
	case <-time.After(20 * time.Millisecond):
	}
	if err := p.withClientSecret(func(string) error { return nil }); !errors.Is(err, secret.ErrCredentialUnavailable) {
		close(release)
		<-useDone
		<-stopDone
		t.Fatalf("new client-secret work during Stop error = %v", err)
	}
	close(release)
	if err := <-useDone; err != nil {
		t.Fatal(err)
	}
	<-stopDone
	if p.scopedClientSecret != (secret.Value{}) || p.scopedRedisPassword != (secret.Value{}) ||
		p.scopedSet || p.clientRSAPrivateKey != nil || p.staticPublicKey != nil ||
		p.sessionSecretKey != [sha256.Size]byte{} || p.activeUses != 0 {
		t.Fatalf(
			"Stop() retained scoped/derived state: scoped=%v active=%d",
			p.scopedSet,
			p.activeUses,
		)
	}
}

func TestOIDCStopWaitsForMaterializationAndPreventsResurrection(t *testing.T) {
	const raw = "$ENV://OIDC_STOP_MATERIALIZE"
	capabilityValue, scope, broker, closeAttempt := newOIDCScopedSecretHarness(
		t, 41, "stop-materialize", map[string]string{raw: "materialized-client-secret"},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call oidcScopedSecretCall) {
		if call.Raw == raw {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: Config{
		ClientID: "apisix", ClientSecret: raw, Discovery: "https://idp.test", BearerOnly: true,
	}}
	materializeDone := make(chan error, 1)
	go func() {
		materializeDone <- base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		)
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(release)
		<-materializeDone
		t.Fatal("Stop() returned before active materialization completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-materializeDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("materialization racing Stop error = %v", err)
	}
	<-stopDone
	if p.scopedSet || p.scopedClientSecret != (secret.Value{}) {
		t.Fatal("materialization resurrected scoped state after Stop")
	}
}

func TestOIDCStopWaitsForProviderConstructionAndSuppressesResult(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fmt.Appendf(nil,
			"{\"issuer\":%q,\"jwks_uri\":%q}", idpURL(r), idpURL(r)+"/jwks",
		))
	}))
	defer server.Close()
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(t, 42, "stop-provider", nil)
	defer closeAttempt()
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: server.URL, BearerOnly: true, UseJWKS: true,
	}}
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	providerDone := make(chan error, 1)
	go func() {
		_, err := p.providerClient(httptest.NewRequest(http.MethodGet, "https://gateway.test", nil))
		providerDone <- err
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		close(release)
		<-providerDone
		t.Fatal("Stop() returned before provider construction completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-providerDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("provider construction racing Stop error = %v", err)
	}
	<-stopDone
	if p.provider != nil || p.client != nil || p.discoveryLoaded {
		t.Fatalf(
			"Stop() retained provider runtime state: provider=%v client=%v discovery=%v",
			p.provider,
			p.client,
			p.discoveryLoaded,
		)
	}
}

func TestOIDCPostInitAfterStopPublishesNothing(t *testing.T) {
	p := &Plugin{config: Config{
		ClientID: "apisix", Discovery: "https://idp.test", BearerOnly: true, UseJWKS: true,
	}}
	capabilityValue, scope, _, closeAttempt := newOIDCScopedSecretHarness(t, 43, "postinit-after-stop", nil)
	defer closeAttempt()
	materializeScopedOIDCSecrets(t, p, capabilityValue, scope)
	p.Stop()
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() after Stop error = %v", err)
	}
	if p.client != nil || p.now != nil || p.config.Scope != "" || p.provider != nil || p.sessionStore != nil {
		t.Fatalf("PostInit() after Stop published state: config=%#v", p.config)
	}
}
