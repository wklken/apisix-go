package dingtalk_auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type dingtalkScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type dingtalkScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	calls  []dingtalkScopedSecretCall
}

func (*dingtalkScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*dingtalkScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *dingtalkScopedSecretBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, dingtalkScopedSecretCall{Scope: scope, Raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private DingTalk test value")
	}
	return value, nil
}

func (*dingtalkScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *dingtalkScopedSecretBroker) scopedCalls() []dingtalkScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]dingtalkScopedSecretCall(nil), broker.calls...)
}

func (broker *dingtalkScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func newDingTalkScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *dingtalkScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID,
		"plugins": map[string]any{name: map[string]any{
			"app_key": config.AppKey, "app_secret": config.AppSecret,
			"secret": config.Secret, "secret_fallbacks": config.SecretFallbacks,
			"redirect_uri": config.RedirectURI,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(
		revision,
		[]generation.Resource{{Key: key, Value: document}},
		nil,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "dingtalk-auth-test",
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
	broker := &dingtalkScopedSecretBroker{values: values}
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
			t.Errorf("close DingTalk scoped attempt: %v", err)
		}
	}
}

func assertDingTalkDescriptor(t *testing.T, got, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if got != want {
		t.Fatalf("descriptor = %q, want %q", got, want)
	}
}

func TestMaterializeScopedSecretsOwnsDingTalkOAuthAndSessionSecrets(t *testing.T) {
	const (
		appSecretRaw = "$ENV://DINGTALK_APP_SECRET"
		sessionRaw   = "$secret://vault/dingtalk/session"
		fallbackOne  = "$ENV://DINGTALK_SESSION_PREVIOUS"
		fallbackTwo  = "$secret://vault/dingtalk/oldest"
	)
	config := Config{
		AppKey: "app-key", AppSecret: appSecretRaw,
		Secret: sessionRaw, SecretFallbacks: []string{fallbackOne, fallbackTwo},
		RedirectURI: "https://login.dingtalk.com/oauth2/auth",
	}
	capabilityValue, scope, broker, closeAttempt := newDingTalkScopedSecretHarness(
		t, 70, "dingtalk-scoped", config, map[string]string{
			appSecretRaw: "resolved-app-secret", sessionRaw: "session-current",
			fallbackOne: "short", fallbackTwo: "session-oldest",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("third-value materialization error = nil")
	}
	if strings.Contains(err.Error(), fallbackOne) || strings.Contains(err.Error(), "short") {
		t.Fatalf("third-value materialization error leaked private data: %v", err)
	}
	if p.config.AppSecret != appSecretRaw || p.config.Secret != sessionRaw ||
		strings.Join(p.config.SecretFallbacks, "|") != strings.Join(config.SecretFallbacks, "|") {
		t.Fatalf("failed materialization changed public config: %#v", p.config)
	}
	if p.client != nil || p.tokenCache != nil || p.appSecretSet ||
		p.appSecret != (secret.Value{}) || p.sessionSecretSet ||
		p.sessionSecret != (secret.Value{}) || len(p.sessionSecretFallbacks) != 0 ||
		p.secretsPrepared {
		t.Fatal("failed materialization installed private, client, or token-cache state")
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit succeeded without installed secrets")
	}

	broker.setValue(fallbackOne, "session-previous")
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("retry materialization error = %v", err)
	}
	assertDingTalkDescriptor(t, p.config.AppSecret, "resolved-app-secret")
	assertDingTalkDescriptor(t, p.config.Secret, "session-current")
	assertDingTalkDescriptor(t, p.config.SecretFallbacks[0], "session-previous")
	assertDingTalkDescriptor(t, p.config.SecretFallbacks[1], "session-oldest")

	calls := broker.scopedCalls()
	wantFields := []string{
		"app_secret", "secret", "secret_fallbacks",
		"app_secret", "secret", "secret_fallbacks", "secret_fallbacks",
	}
	wantRaw := []string{
		appSecretRaw, sessionRaw, fallbackOne,
		appSecretRaw, sessionRaw, fallbackOne, fallbackTwo,
	}
	if len(calls) != len(wantFields) {
		t.Fatalf("resolver calls = %#v, want fields %v", calls, wantFields)
	}
	for i, field := range wantFields {
		wantScope := scope
		wantScope.Field = field
		if calls[i].Scope != wantScope || calls[i].Raw != wantRaw[i] {
			t.Fatalf("resolver call %d = %#v, want scope %#v raw %q", i, calls[i], wantScope, wantRaw[i])
		}
	}

	var tokenBody map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
			t.Fatalf("decode token body: %v", err)
		}
		_, _ = w.Write([]byte(`{"accessToken":"scoped-token"}`))
	}))
	defer api.Close()
	p.config.AccessTokenURL = api.URL
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	token, err := p.fetchAccessToken(httptest.NewRequest(http.MethodGet, "http://gateway.test", nil))
	if err != nil || token != "scoped-token" {
		t.Fatalf("fetchAccessToken() = (%q, %v), want scoped-token", token, err)
	}
	if tokenBody["appKey"] != "app-key" || tokenBody["appSecret"] != "resolved-app-secret" {
		t.Fatalf("token body = %#v, want resolved private app secret", tokenBody)
	}
}

func TestDingTalkSchemaDefersSessionSecretLengthUntilResolution(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"short", "$ENV://X", "$secret://vault/a/path/longer/than/thirty-two/bytes"} {
		config := map[string]any{
			"app_key": "app-key", "app_secret": "$ENV://A",
			"secret": raw, "secret_fallbacks": []any{raw},
			"redirect_uri": "https://login.dingtalk.com/oauth2/auth",
		}
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("schema rejected raw session secret %q before resolution: %v", raw, err)
		}
	}
}

func TestDingTalkResolvedSessionSecretLengthBounds(t *testing.T) {
	tests := []struct {
		name     string
		resolved string
		wantErr  bool
	}{
		{name: "seven rejected", resolved: "1234567", wantErr: true},
		{name: "eight accepted", resolved: "12345678"},
		{name: "thirty two accepted", resolved: strings.Repeat("x", 32)},
		{name: "thirty three rejected", resolved: strings.Repeat("x", 33), wantErr: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const raw = "$ENV://DINGTALK_SESSION_BOUNDARY"
			config := Config{
				AppKey: "app-key", AppSecret: "app-secret", Secret: raw,
				RedirectURI: "https://login.dingtalk.com/oauth2/auth",
			}
			capabilityValue, scope, broker, closeAttempt := newDingTalkScopedSecretHarness(
				t, uint64(80+i), "dingtalk-boundary", config,
				map[string]string{"app-secret": "app-secret", raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if tt.wantErr {
				if err == nil || p.config.Secret != raw || p.secretsPrepared {
					t.Fatalf("resolved length %d result = %v, config=%#v", len(tt.resolved), err, p.config)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertDingTalkDescriptor(t, p.config.Secret, tt.resolved)
			}
			if calls := broker.scopedCalls(); len(calls) != 2 || calls[0].Scope.Field != "app_secret" ||
				calls[1].Scope.Field != "secret" {
				t.Fatalf("boundary resolver calls = %#v, want app_secret then secret only", calls)
			}
		})
	}
}

func TestDingTalkLegacyMaterializationDecryptsContextualSecrets(t *testing.T) {
	const (
		key               = "0123456789abcdef"
		appPlaintext      = "legacy-app-secret"
		sessionPlaintext  = "legacy-session"
		fallbackPlaintext = "legacy-fallback"
	)
	service := testutil.DataEncryptionService(true, []string{key})
	appRaw, err := service.EncryptForContext(appPlaintext, name+".app_secret")
	if err != nil {
		t.Fatal(err)
	}
	sessionRaw, err := service.EncryptForContext(sessionPlaintext, name+".secret")
	if err != nil {
		t.Fatal(err)
	}
	fallbackRaw, err := service.EncryptForContext(fallbackPlaintext, name+".secret_fallbacks")
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{config: Config{
		AppKey: "app-key", AppSecret: appRaw, Secret: sessionRaw,
		SecretFallbacks: []string{fallbackRaw},
		RedirectURI:     "https://login.dingtalk.com/oauth2/auth",
	}}
	p.SetDependencies(base.Dependencies{DataEncryption: service.Resolver()})
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	assertDingTalkDescriptor(t, p.config.AppSecret, appPlaintext)
	assertDingTalkDescriptor(t, p.config.Secret, sessionPlaintext)
	assertDingTalkDescriptor(t, p.config.SecretFallbacks[0], fallbackPlaintext)
	if p.client != nil || p.tokenCache != nil || p.oauthStateReplay != nil {
		t.Fatal("legacy materialization caused PostInit side effects")
	}
}

func TestDingTalkConcurrentScopedMaterializationIsSingleFlight(t *testing.T) {
	config := Config{
		AppKey: "app-key", AppSecret: "$ENV://DINGTALK_CONCURRENT_APP",
		Secret:          "$ENV://DINGTALK_CONCURRENT_SESSION",
		SecretFallbacks: []string{"$ENV://DINGTALK_CONCURRENT_OLD"},
		RedirectURI:     "https://login.dingtalk.com/oauth2/auth",
	}
	capabilityValue, scope, broker, closeAttempt := newDingTalkScopedSecretHarness(
		t, 71, "dingtalk-concurrent", config, map[string]string{
			config.AppSecret: "concurrent-app", config.Secret: "concurrent-session",
			config.SecretFallbacks[0]: "concurrent-old",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	start := make(chan struct{})
	errs := make(chan error, 24)
	var group sync.WaitGroup
	for range 24 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
		})
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := broker.scopedCalls(); len(calls) != 3 {
		t.Fatalf("concurrent resolver calls = %#v, want one ordered materialization", calls)
	}
}

func newScopedDingTalkTestPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (*Plugin, func()) {
	t.Helper()
	capabilityValue, scope, _, closeAttempt := newDingTalkScopedSecretHarness(
		t, revision, resourceID, config, values,
	)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	return p, closeAttempt
}

func TestDingTalkGenerationCachesAndCookiesAreIsolated(t *testing.T) {
	var tokenRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"accessToken":"token-` + body["appSecret"] + `"}`))
	}))
	defer api.Close()
	const (
		appRaw      = "$ENV://DINGTALK_GENERATION_APP"
		sessionRaw  = "$ENV://DINGTALK_GENERATION_SESSION"
		fallbackRaw = "$ENV://DINGTALK_GENERATION_FALLBACK"
	)
	baseConfig := Config{
		AppKey: "shared-app", AppSecret: appRaw, Secret: sessionRaw,
		RedirectURI: "https://login.dingtalk.com/oauth2/auth", AccessTokenURL: api.URL,
	}
	n, closeN := newScopedDingTalkTestPlugin(t, 72, "same-route", baseConfig, map[string]string{
		appRaw: "app-generation-n", sessionRaw: "session-generation-n",
	})
	defer closeN()
	nPlusOne, closeNPlusOne := newScopedDingTalkTestPlugin(
		t, 73, "same-route", baseConfig, map[string]string{
			appRaw: "app-generation-next", sessionRaw: "session-generation-next",
		},
	)
	defer closeNPlusOne()

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	if token, err := n.accessToken(request); err != nil || token != "token-app-generation-n" {
		t.Fatalf("N accessToken() = (%q, %v)", token, err)
	}
	if token, err := nPlusOne.accessToken(request); err != nil || token != "token-app-generation-next" {
		t.Fatalf("N+1 accessToken() = (%q, %v)", token, err)
	}
	if tokenRequests.Load() != 2 || len(n.tokenCache) != 1 || len(nPlusOne.tokenCache) != 1 {
		t.Fatalf(
			"token cache requests/sizes = %d/%d/%d, want 2/1/1",
			tokenRequests.Load(),
			len(n.tokenCache),
			len(nPlusOne.tokenCache),
		)
	}
	for key := range n.tokenCache {
		if key.appSecret != sha256.Sum256([]byte("app-generation-n")) {
			t.Fatal("N token cache key did not use resolved app-secret digest")
		}
	}

	cookie, err := n.sessionCookie(map[string]any{"userid": "generation-n"})
	if err != nil {
		t.Fatal(err)
	}
	requestN := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestN.AddCookie(cookie)
	if user, ok := n.userInfoFromSession(requestN); !ok || user["userid"] != "generation-n" {
		t.Fatalf("N rejected its own cookie: %#v/%t", user, ok)
	}
	requestNext := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestNext.AddCookie(cookie)
	if _, ok := nPlusOne.userInfoFromSession(requestNext); ok {
		t.Fatal("N+1 accepted an unrelated N session cookie")
	}

	rotatedConfig := baseConfig
	rotatedConfig.SecretFallbacks = []string{fallbackRaw}
	rotated, closeRotated := newScopedDingTalkTestPlugin(
		t, 74, "same-route", rotatedConfig, map[string]string{
			appRaw: "app-generation-n", sessionRaw: "session-generation-next",
			fallbackRaw: "session-generation-n",
		},
	)
	defer closeRotated()
	requestRotated := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestRotated.AddCookie(cookie)
	if user, ok := rotated.userInfoFromSession(requestRotated); !ok || user["userid"] != "generation-n" {
		t.Fatal("configured fallback did not verify the previous generation cookie")
	}
	stateResponse := httptest.NewRecorder()
	n.redirectToProvider(
		stateResponse,
		httptest.NewRequest(http.MethodGet, "http://gateway.test", nil),
	)
	stateCookie := findDingTalkStateCookie(stateResponse.Result().Cookies())
	stateRedirect, err := url.Parse(stateResponse.Header().Get("Location"))
	if err != nil || stateCookie == nil {
		t.Fatalf("N OAuth state setup = (%#v, %v)", stateCookie, err)
	}
	rotatedCallback := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.test/callback?state="+url.QueryEscape(stateRedirect.Query().Get("state")),
		nil,
	)
	rotatedCallback.AddCookie(stateCookie)
	if !rotated.verifyAndConsumeOAuthState(rotatedCallback) {
		t.Fatal("configured fallback did not open the previous generation OAuth state")
	}

	n.Stop()
	if len(nPlusOne.tokenCache) != 1 {
		t.Fatal("retiring N cleared N+1 token cache")
	}
	requestNextOwn := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	nextCookie, err := nPlusOne.sessionCookie(map[string]any{"userid": "generation-next"})
	if err != nil {
		t.Fatal(err)
	}
	requestNextOwn.AddCookie(nextCookie)
	if _, ok := nPlusOne.userInfoFromSession(requestNextOwn); !ok {
		t.Fatal("retiring N invalidated N+1 session state")
	}
}

func TestDingTalkStopDrainsActiveRefreshAndPreventsResurrection(t *testing.T) {
	tokenStarted := make(chan struct{})
	releaseToken := make(chan struct{})
	var tokenOnce sync.Once
	var releaseOnce sync.Once
	var providerCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		switch r.URL.Path {
		case "/token":
			tokenOnce.Do(func() { close(tokenStarted) })
			<-releaseToken
			_, _ = w.Write([]byte(`{"accessToken":"active-token"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"errcode":0,"result":{"userid":"active-user"}}`))
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseToken) })
		api.Close()
	})
	p := newTestPlugin(t, Config{
		AppKey: "app-key", AppSecret: "active-app-secret", Secret: "active-session",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token", UserInfoURL: api.URL + "/userinfo",
	})

	handlerDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
		req.Header.Set("X-DingTalk-Code", "active-code")
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})).ServeHTTP(rr, req)
		handlerDone <- rr.Code
	}()
	select {
	case <-tokenStarted:
	case <-time.After(time.Second):
		t.Fatal("active token refresh did not start")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active token refresh drained")
	case <-time.After(20 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseToken) })
	select {
	case code := <-handlerDone:
		if code != http.StatusAccepted {
			t.Fatalf("active handler status = %d, want 202", code)
		}
	case <-time.After(time.Second):
		t.Fatal("active handler did not finish")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after active refresh drained")
	}
	p.Stop()
	if p.client != nil || p.tokenCache != nil || p.oauthStateReplay != nil ||
		p.appSecretSet || p.legacyAppSecret != nil || p.sessionSecretSet ||
		p.legacySessionSecret != nil || len(p.sessionSecretFallbacks) != 0 ||
		len(p.legacySessionFallbacks) != 0 || p.secretsPrepared {
		t.Fatal("Stop retained DingTalk client, cache, replay, or secret state")
	}
	late := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired plugin reached next handler")
	})).ServeHTTP(late, httptest.NewRequest(http.MethodGet, "http://gateway.test", nil))
	if late.Code != http.StatusServiceUnavailable || providerCalls.Load() != 2 {
		t.Fatalf("post-Stop status/provider calls = %d/%d, want 503/2", late.Code, providerCalls.Load())
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop materialization error = %v, want credential unavailable", err)
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

	return p
}

func TestHandlerRedirectsWhenNoSessionAndNoCode(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppKey:      "app-key",
		AppSecret:   "app-secret",
		Secret:      "12345678",
		RedirectURI: "https://login.dingtalk.com/oauth2/auth",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called without session or code")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("response code = %d, want 302", rr.Code)
	}
	location, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if location.Scheme != "https" || location.Host != "login.dingtalk.com" || location.Path != "/oauth2/auth" {
		t.Fatalf("Location = %q, want configured OAuth endpoint", location)
	}
	if location.Query().Get("state") == "" {
		t.Fatal("Location does not contain an OAuth state")
	}
	stateCookie := findDingTalkStateCookie(rr.Result().Cookies())
	if stateCookie == nil || !stateCookie.HttpOnly || stateCookie.MaxAge != 300 {
		t.Fatalf("OAuth state cookie = %#v, want HttpOnly five-minute cookie", stateCookie)
	}
}

func TestHandlerAcceptsQueryCodeOnlyWithValidOAuthState(t *testing.T) {
	var tokenRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			_, _ = w.Write([]byte(`{"accessToken":"access-token-a"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"errcode":0,"result":{"userid":"user-a"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token",
		UserInfoURL:    api.URL + "/userinfo",
	})

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initial,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil),
	)
	stateCookie := findDingTalkStateCookie(initial.Result().Cookies())
	if stateCookie == nil {
		t.Fatal("OAuth state cookie was not set")
	}
	stateRedirect, err := url.Parse(initial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse OAuth redirect: %v", err)
	}

	callback := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state="+url.QueryEscape(stateRedirect.Query().Get("state")),
		nil,
	)
	callback.AddCookie(stateCookie)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })).
		ServeHTTP(rr, callback)
	if !apisixctx.IsSensitiveQueryName(callback, "code") {
		t.Fatal("dingtalk-auth did not register code query key")
	}

	if rr.Code != http.StatusAccepted || tokenRequests != 1 {
		t.Fatalf("callback status/token requests = %d/%d, want 202/1", rr.Code, tokenRequests)
	}
	if got := findDingTalkStateCookie(
		rr.Result().Cookies(),
	); got == nil || got.MaxAge != -1 || got.Path != "/" ||
		got.SameSite != http.SameSiteLaxMode {
		t.Fatalf("state deletion cookie = %#v, want matching expired cookie", got)
	}

	replay := httptest.NewRecorder()
	replayReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state="+url.QueryEscape(stateRedirect.Query().Get("state")),
		nil,
	)
	replayReq.AddCookie(stateCookie)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { t.Fatal("replayed OAuth state reached next") })).
		ServeHTTP(replay, replayReq)
	if replay.Code != http.StatusUnauthorized || tokenRequests != 1 {
		t.Fatalf("replay status/token requests = %d/%d, want 401/1", replay.Code, tokenRequests)
	}
}

func TestHandlerRejectsMissingOrMismatchedOAuthStateBeforeProviderCall(t *testing.T) {
	providerCalls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		_, _ = w.Write([]byte(`{"accessToken":"unexpected"}`))
	}))
	t.Cleanup(api.Close)
	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token",
		UserInfoURL:    api.URL + "/userinfo",
	})

	for _, rawQuery := range []string{"code=code-a", "code=code-a&state=wrong"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback?"+rawQuery, nil)
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("invalid OAuth state reached next") })).
			ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("query %q status = %d, want 401", rawQuery, rr.Code)
		}
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want 0 for invalid OAuth state", providerCalls)
	}
}

func TestHandlerFetchesDingTalkUserInfoAndSetsSession(t *testing.T) {
	var tokenRequests int
	var tokenBody map[string]any
	var userinfoQuery url.Values
	var userinfoBody map[string]any

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			if r.Method != http.MethodPost {
				t.Fatalf("token method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("token Content-Type = %q, want application/json", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
				t.Fatalf("decode token body: %v", err)
			}
			_, _ = w.Write([]byte(`{"accessToken":"access-token-a"}`))
		case "/userinfo":
			if r.Method != http.MethodPost {
				t.Fatalf("userinfo method = %s, want POST", r.Method)
			}
			userinfoQuery = r.URL.Query()
			if err := json.NewDecoder(r.Body).Decode(&userinfoBody); err != nil {
				t.Fatalf("decode userinfo body: %v", err)
			}
			_, _ = w.Write([]byte(`{"errcode":0,"result":{"userid":"user-a","name":"Alice"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token",
		UserInfoURL:    api.URL + "/userinfo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.Header.Set("X-DingTalk-Code", "code-a")
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		rawHeader := r.Header.Get("X-Userinfo")
		if rawHeader == "" {
			t.Fatal("X-Userinfo header is empty")
		}
		decoded, err := base64.StdEncoding.DecodeString(rawHeader)
		if err != nil {
			t.Fatalf("decode X-Userinfo: %v", err)
		}
		if !strings.Contains(string(decoded), `"userid":"user-a"`) {
			t.Fatalf("X-Userinfo = %s, want DingTalk user info", decoded)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called after successful DingTalk auth")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
	if tokenRequests != 1 {
		t.Fatalf("token requests = %d, want 1", tokenRequests)
	}
	if tokenBody["appKey"] != "app-key" || tokenBody["appSecret"] != "app-secret" {
		t.Fatalf("token body = %#v, want appKey/appSecret", tokenBody)
	}
	if userinfoQuery.Get("access_token") != "access-token-a" {
		t.Fatalf("access_token query = %q, want access-token-a", userinfoQuery.Get("access_token"))
	}
	if userinfoBody["code"] != "code-a" {
		t.Fatalf("userinfo code = %q, want code-a", userinfoBody["code"])
	}
	if cookie := findDingTalkSessionCookie(rr.Result().Cookies()); cookie == nil {
		t.Fatal("dingtalk_session cookie was not set")
	}
}

func TestHandlerUsesExistingSessionCookie(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppKey:      "app-key",
		AppSecret:   "app-secret",
		Secret:      "12345678",
		RedirectURI: "https://login.dingtalk.com/oauth2/auth",
	})
	cookie, err := p.sessionCookie(map[string]any{"userid": "cached-user"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("decode X-Userinfo: %v", err)
		}
		if !strings.Contains(string(decoded), `"userid":"cached-user"`) {
			t.Fatalf("X-Userinfo = %s, want cached user", decoded)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called for valid session")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerStoresExternalUserInRequestContext(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppKey:      "app-key",
		AppSecret:   "app-secret",
		Secret:      "12345678",
		RedirectURI: "https://login.dingtalk.com/oauth",
	})
	cookie, err := p.sessionCookie(map[string]any{"userid": "cached-user"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req = apisixctx.WithApisixVars(req, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := apisixctx.GetApisixVar(r, "$external_user").(map[string]any)
		if !ok || user["userid"] != "cached-user" {
			t.Fatalf("$external_user = %#v, want cached DingTalk user", user)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsInvalidDingTalkCode(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"accessToken":"access-token-a"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"errcode":40078,"errmsg":"invalid code"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token",
		UserInfoURL:    api.URL + "/userinfo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.Header.Set("X-DingTalk-Code", "bad-code")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for invalid DingTalk code")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if rr.Body.String() != `{"message":"Invalid authorization code"}` {
		t.Fatalf("response body = %q, want exact invalid code message", rr.Body.String())
	}
}

func TestHandlerCachesAccessToken(t *testing.T) {
	var tokenRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			_, _ = w.Write([]byte(`{"accessToken":"access-token-a"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"errcode":0,"result":{"userid":"` + r.URL.Query().Get("access_token") + `"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		AccessTokenURL: api.URL + "/token",
		UserInfoURL:    api.URL + "/userinfo",
	})

	for _, code := range []string{"code-a", "code-b"} {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
		req.Header.Set("X-DingTalk-Code", code)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})).ServeHTTP(rr, req)
		if rr.Code != http.StatusAccepted {
			t.Fatalf("response code for %s = %d, want 202", code, rr.Code)
		}
	}

	if tokenRequests != 1 {
		t.Fatalf("token requests = %d, want cached access token reused", tokenRequests)
	}
}

func TestSessionCookieSecureByDefault(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppKey:      "app-key",
		AppSecret:   "app-secret",
		Secret:      "12345678",
		RedirectURI: "https://login.dingtalk.com/oauth2/auth",
	})
	cookie, err := p.sessionCookie(map[string]any{"userid": "user-a"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf(
			"cookie attributes = secure:%t httpOnly:%t sameSite:%v",
			cookie.Secure,
			cookie.HttpOnly,
			cookie.SameSite,
		)
	}
}

func TestSessionCookieHonorsCookieControls(t *testing.T) {
	cookieSecure := false
	p := newTestPlugin(t, Config{
		AppKey:         "app-key",
		AppSecret:      "app-secret",
		Secret:         "12345678",
		RedirectURI:    "https://login.dingtalk.com/oauth2/auth",
		CookieSecure:   &cookieSecure,
		CookieSameSite: "Strict",
	})
	cookie, err := p.sessionCookie(map[string]any{"userid": "user-a"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	if cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes = secure:%t sameSite:%v", cookie.Secure, cookie.SameSite)
	}
}

func TestSchemaRequiresSecureCookieForSameSiteNone(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"app_key":          "app-key",
		"app_secret":       "app-secret",
		"secret":           "12345678",
		"redirect_uri":     "https://login.dingtalk.com/oauth2/auth",
		"cookie_same_site": "None",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted SameSite=None without cookie_secure=true")
	}
	config["cookie_secure"] = true
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected secure SameSite=None cookie: %v", err)
	}
}

func findDingTalkSessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == "dingtalk_session" {
			return cookie
		}
	}
	return nil
}

func findDingTalkStateCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == "dingtalk_oauth_state" {
			return cookie
		}
	}
	return nil
}
