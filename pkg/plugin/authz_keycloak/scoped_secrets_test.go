package authz_keycloak

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/wklken/apisix-go/pkg/testutil"
)

type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
	hook   func(scopedSecretCall)
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	call := scopedSecretCall{Scope: scope, Raw: raw}
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

func (broker *scopedSecretBroker) callsSnapshot() []scopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]scopedSecretCall(nil), broker.calls...)
}

func (broker *scopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func (broker *scopedSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
		return
	}
	broker.fail[raw] = err
}

func (broker *scopedSecretBroker) resetCalls() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = nil
}

func (broker *scopedSecretBroker) setHook(hook func(scopedSecretCall)) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.hook = hook
}

func newScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, values map[string]string, keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
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
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &scopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
		PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	baseScope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, baseScope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func scopedKeycloakConfig(endpoint, clientSecret string) Config {
	return Config{
		TokenEndpoint: endpoint,
		ClientID:      "apisix",
		ClientSecret:  clientSecret,
	}
}

func scopedKeycloakDescriptor(plaintext string) string {
	return fmt.Sprintf("plugin_config#sha256:%x", sha256.Sum256([]byte(plaintext)))
}

func materializeScopedKeycloak(
	t *testing.T,
	p *Plugin,
	secrets secret.GenerationSecrets,
	scope secret.Scope,
) {
	t.Helper()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
}

func assertScopedKeycloakCall(
	t *testing.T, scope secret.Scope, calls []scopedSecretCall, raw string,
) {
	t.Helper()
	wantScope := scope
	wantScope.Field = "client_secret"
	if !strings.HasPrefix(raw, "$secret://") && !strings.HasPrefix(strings.ToUpper(raw), "$ENV://") {
		if len(calls) != 0 {
			t.Fatalf("broker calls = %#v, want none for literal or ciphertext", calls)
		}
		return
	}
	if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != raw {
		t.Fatalf("broker calls = %#v, want scope %#v and raw %q", calls, wantScope, raw)
	}
}

func TestScopedSecretsMaterializeKeycloakClientSecret(t *testing.T) {
	const (
		raw       = "$ENV://AUTHZ_KEYCLOAK_SCOPED_CLIENT_SECRET"
		plaintext = "scoped-client-secret"
	)
	forms := make(chan url.Values, 1)
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		forms <- r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"service-token","expires_in":300}`))
	}))
	t.Cleanup(keycloak.Close)

	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 7, "materialize", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: scopedKeycloakConfig(keycloak.URL, raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeScopedKeycloak(t, p, secrets, scope)
	if got := p.config.ClientSecret; got != scopedKeycloakDescriptor(plaintext) {
		t.Fatalf("client_secret descriptor = %q, want resolved content descriptor", got)
	}
	assertScopedKeycloakCall(t, scope, broker.callsSnapshot(), raw)
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	if token, err := p.serviceAccountAccessToken(); err != nil || token != "service-token" {
		t.Fatalf("serviceAccountAccessToken() = %q, %v", token, err)
	}
	if got := (<-forms).Get("client_secret"); got != plaintext {
		t.Fatalf("client_secret form = %q, want resolved value", got)
	}
	for _, sensitive := range []string{raw, plaintext} {
		if strings.Contains(fmt.Sprintf("%#v", p.config), sensitive) {
			t.Fatalf("effective config retained %q: %#v", sensitive, p.config)
		}
	}
}

func TestScopedSecretsSkipEmptyKeycloakClientSecret(t *testing.T) {
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, 8, "empty", nil)
	defer closeAttempt()
	p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", "")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeScopedKeycloak(t, p, secrets, scope)
	if calls := broker.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("empty optional client secret broker calls = %#v, want none", calls)
	}
	if p.config.ClientSecret != "" {
		t.Fatalf("empty client_secret changed to %q", p.config.ClientSecret)
	}
	var got string
	if err := p.withClientSecret(func(clientSecret string) error {
		got = clientSecret
		return nil
	}); err != nil {
		t.Fatalf("withClientSecret() error = %v", err)
	}
	if got != "" {
		t.Fatalf("withClientSecret() value = %q, want empty", got)
	}
	if key := p.serviceAccountCacheKey("http://keycloak.test/token"); key == "" {
		t.Fatal("empty optional client secret produced empty cache identity")
	}
}

func TestScopedSecretsResolveManagedKeycloakClientSecret(t *testing.T) {
	const (
		raw       = "$secret://vault/keycloak/client-secret"
		plaintext = "managed-client-secret"
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 9, "managed", map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeScopedKeycloak(t, p, secrets, scope)
	assertScopedKeycloakCall(t, scope, broker.callsSnapshot(), raw)
	if p.config.ClientSecret != scopedKeycloakDescriptor(plaintext) {
		t.Fatalf("managed descriptor = %q", p.config.ClientSecret)
	}
	if err := p.withClientSecret(func(clientSecret string) error {
		if clientSecret != plaintext {
			t.Fatalf("managed client secret = %q, want resolved value", clientSecret)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScopedKeycloakCacheIdentityUsesDigestNotCredential(t *testing.T) {
	const raw = "$ENV://AUTHZ_KEYCLOAK_CACHE_IDENTITY"
	keys := make([]string, 0, 2)
	for index, plaintext := range []string{"cache-secret-a", "cache-secret-b"} {
		secrets, scope, _, closeAttempt := newScopedSecretHarness(
			t, uint64(10+index), fmt.Sprintf("cache-%d", index), map[string]string{raw: plaintext},
		)
		p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		materializeScopedKeycloak(t, p, secrets, scope)
		key := p.serviceAccountCacheKey("http://keycloak.test/token")
		for _, sensitive := range []string{raw, plaintext, p.config.ClientSecret} {
			if strings.Contains(key, sensitive) {
				t.Fatalf("cache identity %q contains %q", key, sensitive)
			}
		}
		keys = append(keys, key)
		closeAttempt()
	}
	if keys[0] == keys[1] {
		t.Fatal("cache identity did not change with resolved client secret")
	}
}

func TestScopedSecretsKeycloakModesUseResolvedContentDescriptors(t *testing.T) {
	encryption := testutil.DataEncryptionService(true, []string{"0123456789abcdef"})
	contextual, err := encryption.EncryptForContext("contextual-client-secret", name+".client_secret")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	for index, test := range []struct {
		name      string
		raw       string
		plaintext string
	}{
		{name: "literal", raw: "literal-client-secret", plaintext: "literal-client-secret"},
		{name: "contextual ciphertext", raw: contextual, plaintext: "contextual-client-secret"},
		{name: "environment", raw: "$ENV://KEYCLOAK_MODE_SECRET", plaintext: "environment-client-secret"},
		{name: "managed", raw: "$secret://vault/keycloak/mode-secret", plaintext: "managed-client-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			secrets, scope, broker, closeAttempt := newScopedSecretHarness(
				t, uint64(20+index), fmt.Sprintf("mode-%d", index),
				map[string]string{test.raw: test.plaintext},
				"0123456789abcdef",
			)
			defer closeAttempt()
			p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", test.raw)}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			materializeScopedKeycloak(t, p, secrets, scope)
			assertScopedKeycloakCall(t, scope, broker.callsSnapshot(), test.raw)
			if got := p.config.ClientSecret; got != scopedKeycloakDescriptor(test.plaintext) {
				t.Fatalf("descriptor = %q, want resolved content digest", got)
			}
			for _, sensitive := range []string{test.raw, test.plaintext} {
				if strings.Contains(fmt.Sprintf("%#v", p.config), sensitive) {
					t.Fatalf("effective config retained %q: %#v", sensitive, p.config)
				}
			}
		})
	}
}

func TestScopedSecretsKeycloakFailureAndBlankAreAtomicAndRetryable(t *testing.T) {
	const raw = "$ENV://KEYCLOAK_RETRY_SECRET"
	for _, test := range []struct {
		name       string
		value      string
		brokerFail bool
	}{
		{name: "broker failure", value: "resolved-on-retry", brokerFail: true},
		{name: "resolved empty", value: ""},
		{name: "resolved whitespace", value: " \t\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			secrets, scope, broker, closeAttempt := newScopedSecretHarness(
				t, 30, "retry-"+strings.ReplaceAll(test.name, " ", "-"), map[string]string{raw: test.value},
			)
			defer closeAttempt()
			if test.brokerFail {
				broker.setFailure(raw, errors.New("broker failure contains "+raw))
			}
			p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
			if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
				t.Fatalf("first materialization error = %v, want redacted credential unavailable", err)
			}
			for _, sensitive := range []string{raw, test.value, "broker failure"} {
				if sensitive != "" && strings.Contains(err.Error(), sensitive) {
					t.Fatalf("materialization error %q contains %q", err, sensitive)
				}
			}
			if p.config.ClientSecret != raw || p.scopedSet || p.scopedClientSecret != (secret.Value{}) {
				t.Fatalf("failed materialization installed state: config=%#v", p.config)
			}
			broker.setFailure(raw, nil)
			broker.setValue(raw, "retry-client-secret")
			broker.resetCalls()
			materializeScopedKeycloak(t, p, secrets, scope)
			assertScopedKeycloakCall(t, scope, broker.callsSnapshot(), raw)
			if p.config.ClientSecret != scopedKeycloakDescriptor("retry-client-secret") {
				t.Fatalf("retry descriptor = %q", p.config.ClientSecret)
			}
		})
	}
}

func TestScopedSecretsKeycloakConcurrentMaterializationIsSingleFlight(t *testing.T) {
	const (
		raw     = "$ENV://KEYCLOAK_SINGLEFLIGHT_SECRET"
		workers = 32
	)
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 40, "singleflight", map[string]string{raw: "singleflight-client-secret"},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(scopedSecretCall) {
		once.Do(func() { close(entered) })
		<-release
	})
	p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		go func() {
			ready.Done()
			<-start
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped materialization leader")
	}
	close(release)
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent scoped materialization error = %v", err)
		}
	}
	assertScopedKeycloakCall(t, scope, broker.callsSnapshot(), raw)
}

func TestPostInitDoesNotResolveKeycloakClientSecret(t *testing.T) {
	const raw = "$ENV://KEYCLOAK_POST_INIT_MUST_NOT_RESOLVE"
	t.Setenv("KEYCLOAK_POST_INIT_MUST_NOT_RESOLVE", "must-not-be-read")
	p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := p.PostInit()
	if !errors.Is(err, errKeycloakCredentialsUnavailable) {
		t.Fatalf("PostInit() error = %v, want unprepared credential failure", err)
	}
	if p.config.ClientSecret != raw || p.scopedSet {
		t.Fatalf("PostInit() resolved or changed client secret state: %#v", p.config)
	}
}

func TestKeycloakGenerationInstancesDoNotCrossUseClientSecrets(t *testing.T) {
	seen := make(chan string, 2)
	keycloak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
			return
		}
		seen <- r.PostForm.Get("client_secret")
		_, _ = w.Write([]byte(`{"access_token":"token","expires_in":300}`))
	}))
	t.Cleanup(keycloak.Close)

	plugins := make([]*Plugin, 0, 2)
	closures := make([]func(), 0, 2)
	for index, plaintext := range []string{"generation-n-secret", "generation-n-plus-one-secret"} {
		raw := fmt.Sprintf("$ENV://KEYCLOAK_GENERATION_%d", index)
		secrets, scope, _, closeAttempt := newScopedSecretHarness(
			t, uint64(50+index), fmt.Sprintf("generation-%d", index), map[string]string{raw: plaintext},
		)
		p := &Plugin{config: scopedKeycloakConfig(keycloak.URL, raw)}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		materializeScopedKeycloak(t, p, secrets, scope)
		if err := p.PostInit(); err != nil {
			t.Fatal(err)
		}
		plugins = append(plugins, p)
		closures = append(closures, closeAttempt)
	}
	defer closures[0]()
	defer closures[1]()
	defer plugins[0].Stop()
	defer plugins[1].Stop()

	var wait sync.WaitGroup
	wait.Add(2)
	for _, p := range plugins {
		go func(p *Plugin) {
			defer wait.Done()
			if _, err := p.serviceAccountAccessToken(); err != nil {
				t.Errorf("serviceAccountAccessToken() error = %v", err)
			}
		}(p)
	}
	wait.Wait()
	got := []string{<-seen, <-seen}
	if (got[0] != "generation-n-secret" || got[1] != "generation-n-plus-one-secret") &&
		(got[1] != "generation-n-secret" || got[0] != "generation-n-plus-one-secret") {
		t.Fatalf("generation secrets = %#v", got)
	}
	plugins[0].Stop()
	var n1 string
	if err := plugins[1].withClientSecret(func(value string) error { n1 = value; return nil }); err != nil {
		t.Fatalf("N+1 credential after N stop error = %v", err)
	}
	if n1 != "generation-n-plus-one-secret" {
		t.Fatalf("N+1 credential after N stop = %q", n1)
	}
}

type trackingKeycloakBody struct {
	*strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (body *trackingKeycloakBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

type blockingKeycloakTransport struct {
	entered chan *http.Request
	release chan struct{}
	body    *trackingKeycloakBody
}

func (transport *blockingKeycloakTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.entered <- request
	<-transport.release
	transport.body = &trackingKeycloakBody{
		Reader: strings.NewReader(`{"access_token":"service-token","expires_in":300}`),
		closed: make(chan struct{}),
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       transport.body,
		Request:    request,
	}, nil
}

func prepareKeycloakStopPlugin(t *testing.T, endpoint, clientSecret string) (*Plugin, func()) {
	t.Helper()
	p := &Plugin{config: scopedKeycloakConfig(endpoint, clientSecret)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	raw := "$ENV://KEYCLOAK_STOP_SCOPED"
	p.config.ClientSecret = raw
	secrets, scope, _, closeScopedAttempt := newScopedSecretHarness(
		t, 60, "stop-scoped", map[string]string{raw: clientSecret},
	)
	materializeScopedKeycloak(t, p, secrets, scope)
	if err := p.PostInit(); err != nil {
		closeScopedAttempt()
		t.Fatal(err)
	}
	return p, closeScopedAttempt
}

func waitForKeycloakRetirement(t *testing.T, p *Plugin) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		p.credentialMu.Lock()
		retired := p.retired
		activeUses := p.activeUses
		p.credentialMu.Unlock()
		if retired && activeUses == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for retired Keycloak plugin with one active credential use")
}

func TestKeycloakTokenFormsStayInsideCredentialCallbackAndStopBarrier(t *testing.T) {
	for _, mode := range []string{"scoped"} {
		for _, grant := range []string{"client_credentials", "refresh_token"} {
			t.Run(mode+"/"+grant, func(t *testing.T) {
				clientSecret := mode + "-" + grant + "-client-secret"
				endpoint := "http://keycloak-" + mode + "-" + grant + ".test/token"
				p, closeAttempt := prepareKeycloakStopPlugin(t, endpoint, clientSecret)
				defer closeAttempt()
				sharedCache.Lock()
				delete(sharedCache.serviceAccountToken, p.serviceAccountCacheKey(endpoint))
				sharedCache.Unlock()
				transport := &blockingKeycloakTransport{
					entered: make(chan *http.Request, 1),
					release: make(chan struct{}),
				}
				p.client.SetTransport(transport)
				neutralClient := p.client
				clientReleased := make(chan struct{})
				p.credentialMu.Lock()
				originalRelease := p.clientRelease
				p.clientRelease = func() {
					originalRelease()
					close(clientReleased)
				}
				p.credentialMu.Unlock()
				if grant == "refresh_token" {
					p.serviceAccountToken = tokenCache{
						value:                 "expired-token",
						expiresAt:             time.Now().Add(-time.Second),
						refreshToken:          "refresh-token-for-" + mode,
						refreshTokenExpiresAt: time.Now().Add(time.Hour),
					}
				}

				requestDone := make(chan error, 1)
				go func() {
					_, err := p.serviceAccountAccessToken()
					requestDone <- err
				}()
				var retainedRequest *http.Request
				select {
				case retainedRequest = <-transport.entered:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for Keycloak token request")
				}
				formBody, err := io.ReadAll(retainedRequest.Body)
				if err != nil {
					t.Fatalf("read outbound form: %v", err)
				}
				form, err := url.ParseQuery(string(formBody))
				if err != nil {
					t.Fatalf("parse outbound form: %v", err)
				}
				if form.Get("client_secret") != clientSecret || form.Get("grant_type") != grant {
					t.Fatalf("outbound form = %#v, want grant %q and scoped credential", form, grant)
				}
				if grant == "refresh_token" && form.Get("refresh_token") != "refresh-token-for-"+mode {
					t.Fatalf("refresh token form = %#v", form)
				}

				firstStop := make(chan struct{})
				secondStop := make(chan struct{})
				go func() { p.Stop(); close(firstStop) }()
				go func() { p.Stop(); close(secondStop) }()
				waitForKeycloakRetirement(t, p)
				select {
				case <-clientReleased:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for neutral client release during credential drain")
				}
				for _, stopDone := range []chan struct{}{firstStop, secondStop} {
					select {
					case <-stopDone:
						t.Fatal("Stop() returned before token request left credential callback")
					default:
					}
				}
				if err := p.withClientSecret(
					func(string) error { return nil },
				); !errors.Is(
					err,
					errKeycloakCredentialsUnavailable,
				) {
					t.Fatalf("credential use after retirement error = %v", err)
				}

				close(transport.release)
				if err := <-requestDone; err != nil {
					t.Fatalf("token request error = %v", err)
				}
				<-firstStop
				<-secondStop
				p.Stop()
				select {
				case <-transport.body.closed:
				default:
					t.Fatal("token response body was not closed")
				}
				if retainedRequest.GetBody != nil {
					t.Fatal("retained token request still exposes GetBody")
				}
				if retainedRequest.Body != http.NoBody {
					t.Fatalf("retained token request body = %T, want http.NoBody", retainedRequest.Body)
				}
				for key, values := range retainedRequest.Header {
					if strings.Contains(key, clientSecret) || strings.Contains(strings.Join(values, ""), clientSecret) {
						t.Fatalf("retained request header exposes credential: %s=%#v", key, values)
					}
				}
				for _, retained := range []string{
					neutralClient.Token,
					neutralClient.AuthScheme,
					neutralClient.FormData.Encode(),
					neutralClient.QueryParam.Encode(),
					fmt.Sprint(neutralClient.Header),
					fmt.Sprint(neutralClient.UserInfo),
				} {
					if strings.Contains(retained, clientSecret) {
						t.Fatalf("neutral Resty client retained credential in %q", retained)
					}
				}
				if bytes.Contains(formBody, []byte(p.config.ClientSecret)) {
					t.Fatalf("outbound form contains public descriptor %q", p.config.ClientSecret)
				}
				if token, err := p.serviceAccountAccessToken(); !errors.Is(err, errKeycloakCredentialsUnavailable) ||
					token != "" {
					t.Fatalf("service account work after Stop() = %q, %v", token, err)
				}
				p.credentialMu.Lock()
				stateRetained := p.client != nil || p.scopedClientSecret != (secret.Value{}) ||
					p.clientSecretDigest != [sha256.Size]byte{} || p.scopedSet || !p.retired || p.activeUses != 0
				p.credentialMu.Unlock()
				if stateRetained {
					t.Fatal("Stop() retained Keycloak credential/client state")
				}
			})
		}
	}
}

func TestScopedSecretsKeycloakStopDuringMaterializeCannotRevive(t *testing.T) {
	const raw = "$ENV://KEYCLOAK_STOP_DURING_MATERIALIZE"
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(
		t, 70, "stop-materialize", map[string]string{raw: "late-client-secret"},
	)
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(scopedSecretCall) {
		once.Do(func() { close(entered) })
		<-release
	})
	p := &Plugin{config: scopedKeycloakConfig("http://keycloak.test/token", raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped materializer")
	}
	p.Stop()
	close(release)
	if err := <-done; err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("materialization racing Stop() error = %v", err)
	}
	if p.config.ClientSecret != raw || p.scopedClientSecret != (secret.Value{}) || p.scopedSet {
		t.Fatalf("materialization revived stopped state: %#v", p.config)
	}
	callCount := len(broker.callsSnapshot())
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err == nil {
		t.Fatal("materialization after Stop() error = nil")
	}
	if got := len(broker.callsSnapshot()); got != callCount {
		t.Fatalf("broker calls after Stop() = %d, want %d", got, callCount)
	}
}
