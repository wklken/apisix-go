package feishu_auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
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

type feishuScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type retainingFeishuOAuthTransport struct {
	request *http.Request
	body    string
}

func (transport *retainingFeishuOAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	transport.request = request
	transport.body = string(body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`{"access_token":"retained-token","expires_in":7200}`,
		)),
		Request: request,
	}, nil
}

type feishuScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	calls  []feishuScopedSecretCall
}

func (broker *feishuScopedSecretBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, feishuScopedSecretCall{Scope: scope, Raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Feishu test value")
	}
	return value, nil
}

func (broker *feishuScopedSecretBroker) scopedCalls() []feishuScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]feishuScopedSecretCall(nil), broker.calls...)
}

func (broker *feishuScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func newFeishuScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *feishuScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID,
		"plugins": map[string]any{name: map[string]any{
			"app_id": config.AppID, "app_secret": config.AppSecret,
			"secret": config.Secret, "secret_fallbacks": config.SecretFallbacks,
			"auth_redirect_uri": config.AuthRedirectURI, "redirect_uri": config.RedirectURI,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "feishu-auth-test",
		}},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &feishuScopedSecretBroker{values: values}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close Feishu scoped generation: %v", err)
		}
	}
}

func assertFeishuDescriptor(t *testing.T, got, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if got != want {
		t.Fatalf("descriptor = %q, want %q", got, want)
	}
}

func TestMaterializeScopedSecretsOwnsFeishuOAuthAndSessionSecrets(t *testing.T) {
	const (
		appSecretRaw = "$ENV://FEISHU_APP_SECRET"
		sessionRaw   = "$secret://vault/feishu/session"
		fallbackOne  = "$ENV://FEISHU_SESSION_PREVIOUS"
		fallbackTwo  = "$secret://vault/feishu/oldest"
	)
	config := Config{
		AppID: "app-id", AppSecret: appSecretRaw,
		Secret: sessionRaw, SecretFallbacks: []string{fallbackOne, fallbackTwo},
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	}
	secrets, scope, broker, closeAttempt := newFeishuScopedSecretHarness(
		t, 70, "feishu-scoped", config, map[string]string{
			appSecretRaw: "resolved-app-secret", sessionRaw: "会话密钥八个字符",
			fallbackOne: "short", fallbackTwo: "session-oldest",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
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
	if p.client != nil || p.appSecretSet ||
		p.appSecret != (secret.Value{}) || p.sessionSecretSet ||
		p.sessionSecret != (secret.Value{}) || len(p.sessionSecretFallbacks) != 0 ||
		p.secretsPrepared {
		t.Fatal("failed materialization installed private, client, or replay state")
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit succeeded without installed secrets")
	}

	broker.setValue(fallbackOne, "session-previous")
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("retry materialization error = %v", err)
	}
	assertFeishuDescriptor(t, p.config.AppSecret, "resolved-app-secret")
	assertFeishuDescriptor(t, p.config.Secret, "会话密钥八个字符")
	assertFeishuDescriptor(t, p.config.SecretFallbacks[0], "session-previous")
	assertFeishuDescriptor(t, p.config.SecretFallbacks[1], "session-oldest")

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
		_, _ = w.Write([]byte(`{"access_token":"scoped-token","expires_in":7200}`))
	}))
	defer api.Close()
	p.config.AccessTokenURL = api.URL
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	token, err := p.fetchAccessToken(httptest.NewRequest(http.MethodGet, "http://gateway.test", nil), "code-a")
	if err != nil || token != "scoped-token" {
		t.Fatalf("fetchAccessToken() = (%q, %v), want scoped-token", token, err)
	}
	if tokenBody["client_id"] != "app-id" || tokenBody["client_secret"] != "resolved-app-secret" {
		t.Fatalf("token body = %#v, want resolved private app secret", tokenBody)
	}
}

func TestFeishuOAuthRequestDoesNotRetainAppSecretBody(t *testing.T) {
	const appSecret = "retained-app-secret"
	p := newTestPlugin(t, Config{
		AppID: "app-id", AppSecret: appSecret, Secret: "session-secret",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  "http://feishu.invalid/token",
	})
	transport := &retainingFeishuOAuthTransport{}
	p.client = &http.Client{Transport: transport}

	token, err := p.fetchAccessToken(
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback", nil),
		"code-a",
	)
	if err != nil || token != "retained-token" {
		t.Fatalf("fetchAccessToken() = (%q, %v), want retained-token", token, err)
	}
	if !strings.Contains(transport.body, appSecret) {
		t.Fatalf("OAuth request body = %q, want private app secret during send", transport.body)
	}
	if transport.request == nil {
		t.Fatal("retaining transport did not observe OAuth request")
	}
	if transport.request.Body != http.NoBody || transport.request.GetBody != nil {
		t.Fatalf(
			"retained OAuth body = %#v GetBody present = %t, want scrubbed",
			transport.request.Body,
			transport.request.GetBody != nil,
		)
	}
	if strings.Contains(transport.request.URL.String(), appSecret) ||
		strings.Contains(transport.request.Header.Get("Authorization"), appSecret) {
		t.Fatalf("retained OAuth request exposes app secret: %#v", transport.request)
	}
}

func TestFeishuDerivedOAuthAndSessionPayloadsAreCleared(t *testing.T) {
	const (
		sessionSecret = "session-secret"
		fingerprint   = "feishu-derived-test"
	)
	now := time.Unix(1_800_000_000, 0)

	t.Run("verified session payload", func(t *testing.T) {
		payload := []byte(`{"userinfo":{"open_id":"ou-a"},"expires_at":1800000060}`)
		sealed, err := base.SealOAuthSession(
			payload, sessionSecret, fingerprint, now, now.Add(time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		var retained []byte
		err = useVerifiedFeishuSessionPayload(
			sealed, sessionSecret, nil, fingerprint, now,
			func(verified []byte) error {
				retained = verified
				var session sessionPayload
				if err := json.Unmarshal(verified, &session); err != nil {
					return err
				}
				if session.UserInfo["open_id"] != "ou-a" {
					t.Fatalf("verified session = %#v", session)
				}
				return nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		assertFeishuBytesCleared(t, retained)
	})

	t.Run("session sealing input", func(t *testing.T) {
		payload := []byte("session-sealing-input")
		sealed, err := sealAndClearFeishuSessionPayload(
			payload, sessionSecret, fingerprint, now, now.Add(time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := base.OpenOAuthSession(sealed, sessionSecret, nil, fingerprint, now)
		if err != nil || string(opened) != "session-sealing-input" {
			t.Fatalf("sealed payload open = %q/%v", opened, err)
		}
		clear(opened)
		assertFeishuBytesCleared(t, payload)
	})
}

func assertFeishuBytesCleared(t *testing.T, payload []byte) {
	t.Helper()
	if len(payload) == 0 {
		t.Fatal("derived payload was not retained by the test")
	}
	for index, value := range payload {
		if value != 0 {
			t.Fatalf("derived payload byte %d = %d, want zero", index, value)
		}
	}
}

func TestFeishuSchemaDefersSessionSecretLengthUntilResolution(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"short", "$ENV://X", "$secret://vault/a/path/longer/than/thirty-two/bytes"} {
		config := map[string]any{
			"app_id": "app-id", "app_secret": "$ENV://A",
			"secret": raw, "secret_fallbacks": []any{raw},
			"auth_redirect_uri": "https://gateway.example.com/callback",
			"redirect_uri":      "https://login.feishu.cn/oauth",
		}
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("schema rejected raw session secret %q before resolution: %v", raw, err)
		}
	}
}

func TestFeishuResolvedSessionSecretRuneCountBounds(t *testing.T) {
	tests := []struct {
		name     string
		resolved string
		wantErr  bool
	}{
		{name: "seven runes rejected", resolved: "会话密钥七字符", wantErr: true},
		{name: "eight runes accepted", resolved: "会话密钥八个字符"},
		{name: "thirty two runes accepted", resolved: strings.Repeat("界", 32)},
		{name: "thirty three runes rejected", resolved: strings.Repeat("界", 33), wantErr: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const raw = "$ENV://FEISHU_SESSION_BOUNDARY"
			config := Config{
				AppID: "app-id", AppSecret: "app-secret", Secret: raw,
				AuthRedirectURI: "https://gateway.example.com/callback",
				RedirectURI:     "https://login.feishu.cn/oauth",
			}
			secrets, scope, broker, closeAttempt := newFeishuScopedSecretHarness(
				t, uint64(80+i), "feishu-boundary", config,
				map[string]string{"app-secret": "app-secret", raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
			if tt.wantErr {
				if err == nil || p.config.Secret != raw || p.secretsPrepared {
					t.Fatalf("resolved rune count %d result = %v, config=%#v", len([]rune(tt.resolved)), err, p.config)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertFeishuDescriptor(t, p.config.Secret, tt.resolved)
			}
			if calls := broker.scopedCalls(); len(calls) != 1 || calls[0].Scope.Field != "secret" {
				t.Fatalf("boundary resolver calls = %#v, want secret reference only", calls)
			}
		})
	}
}

func TestFeishuConcurrentScopedMaterializationIsSingleFlight(t *testing.T) {
	config := Config{
		AppID: "app-id", AppSecret: "$ENV://FEISHU_CONCURRENT_APP",
		Secret:          "$ENV://FEISHU_CONCURRENT_SESSION",
		SecretFallbacks: []string{"$ENV://FEISHU_CONCURRENT_OLD"},
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	}
	secrets, scope, broker, closeAttempt := newFeishuScopedSecretHarness(
		t, 71, "feishu-concurrent", config, map[string]string{
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
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
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

func newScopedFeishuTestPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (*Plugin, func()) {
	t.Helper()
	secrets, scope, _, closeAttempt := newFeishuScopedSecretHarness(
		t, revision, resourceID, config, values,
	)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	return p, func() {
		p.Stop()
		closeAttempt()
	}
}

func TestFeishuGenerationOAuthAndSessionStateIsIsolated(t *testing.T) {
	var tokenRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"access_token":"token-` + body["client_secret"] + `","expires_in":7200}`))
	}))
	defer api.Close()
	const (
		appRaw      = "$ENV://FEISHU_GENERATION_APP"
		sessionRaw  = "$ENV://FEISHU_GENERATION_SESSION"
		fallbackRaw = "$ENV://FEISHU_GENERATION_FALLBACK"
	)
	baseConfig := Config{
		AppID: "shared-app", AppSecret: appRaw, Secret: sessionRaw,
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth", AccessTokenURL: api.URL,
	}
	n, closeN := newScopedFeishuTestPlugin(t, 72, "same-route", baseConfig, map[string]string{
		appRaw: "app-generation-n", sessionRaw: "session-generation-n",
	})
	defer closeN()
	nPlusOne, closeNPlusOne := newScopedFeishuTestPlugin(
		t, 73, "same-route", baseConfig, map[string]string{
			appRaw: "app-generation-next", sessionRaw: "session-generation-next",
		},
	)
	defer closeNPlusOne()

	request := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	if token, err := n.fetchAccessToken(request, "code-n"); err != nil || token != "token-app-generation-n" {
		t.Fatalf("N fetchAccessToken() = (%q, %v)", token, err)
	}
	if token, err := nPlusOne.fetchAccessToken(
		request,
		"code-next",
	); err != nil ||
		token != "token-app-generation-next" {
		t.Fatalf("N+1 fetchAccessToken() = (%q, %v)", token, err)
	}
	if tokenRequests.Load() != 2 {
		t.Fatalf("token requests = %d, want generation-private requests", tokenRequests.Load())
	}

	cookie, err := n.sessionCookie(map[string]any{"open_id": "generation-n"})
	if err != nil {
		t.Fatal(err)
	}
	requestN := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestN.AddCookie(cookie)
	if user, ok := n.userInfoFromSession(requestN); !ok || user["open_id"] != "generation-n" {
		t.Fatalf("N rejected its own cookie: %#v/%t", user, ok)
	}
	requestNext := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestNext.AddCookie(cookie)
	if _, ok := nPlusOne.userInfoFromSession(requestNext); ok {
		t.Fatal("N+1 accepted an unrelated N session cookie")
	}

	rotatedConfig := baseConfig
	rotatedConfig.SecretFallbacks = []string{fallbackRaw}
	rotated, closeRotated := newScopedFeishuTestPlugin(
		t, 74, "same-route", rotatedConfig, map[string]string{
			appRaw: "app-generation-n", sessionRaw: "session-generation-next",
			fallbackRaw: "session-generation-n",
		},
	)
	defer closeRotated()
	requestRotated := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestRotated.AddCookie(cookie)
	if user, ok := rotated.userInfoFromSession(requestRotated); !ok || user["open_id"] != "generation-n" {
		t.Fatal("configured fallback did not verify the previous generation cookie")
	}
	n.Stop()
	nextCookie, err := nPlusOne.sessionCookie(map[string]any{"open_id": "generation-next"})
	if err != nil {
		t.Fatal(err)
	}
	requestNextOwn := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	requestNextOwn.AddCookie(nextCookie)
	if _, ok := nPlusOne.userInfoFromSession(requestNextOwn); !ok {
		t.Fatal("retiring N invalidated N+1 session state")
	}
}

func TestFeishuStopDrainsActiveRequestAndPreventsResurrection(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"access_token":"active-token","expires_in":7200}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"active-user"}}`))
		}
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseToken) })
		api.Close()
	})
	p := newTestPlugin(t, Config{
		AppID: "app-id", AppSecret: "active-app-secret", Secret: "active-session",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  api.URL + "/token", UserInfoURL: api.URL + "/userinfo",
	})

	handlerDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
		req.Header.Set("X-Feishu-Code", "active-code")
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})).ServeHTTP(rr, req)
		handlerDone <- rr.Code
	}()
	select {
	case <-tokenStarted:
	case <-time.After(time.Second):
		t.Fatal("active token request did not start")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active token request drained")
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
		t.Fatal("Stop did not finish after active request drained")
	}
	p.Stop()
	if p.client != nil || p.appSecretSet ||
		p.sessionSecretSet || len(p.sessionSecretFallbacks) != 0 ||
		p.secretsPrepared {
		t.Fatal("Stop retained Feishu client or secret state")
	}
	late := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired plugin reached next handler")
	})).ServeHTTP(late, httptest.NewRequest(http.MethodGet, "http://gateway.test", nil))
	if late.Code != http.StatusServiceUnavailable || providerCalls.Load() != 2 {
		t.Fatalf("post-Stop status/provider calls = %d/%d, want 503/2", late.Code, providerCalls.Load())
	}
	secrets, scope, cleanup := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop materialization error = %v, want credential unavailable", err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop PostInit error = %v, want credential unavailable", err)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	values := map[string]string{
		cfg.AppSecret: cfg.AppSecret,
		cfg.Secret:    cfg.Secret,
	}
	for _, raw := range cfg.SecretFallbacks {
		values[raw] = raw
	}
	secrets, scope, _, cleanup := newFeishuScopedSecretHarness(t, 1, "test-route", cfg, values)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitAppliesOfficialDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	})

	if p.config.CodeHeader != "X-Feishu-Code" || p.config.CodeQuery != "code" {
		t.Fatalf("code locations = (%q, %q), want official defaults", p.config.CodeHeader, p.config.CodeQuery)
	}
	if p.config.AccessTokenURL != defaultAccessTokenURL || p.config.UserInfoURL != defaultUserInfoURL {
		t.Fatalf("provider URLs = (%q, %q), want official defaults", p.config.AccessTokenURL, p.config.UserInfoURL)
	}
	if p.config.Timeout != 6000 || p.client.Timeout != 6*time.Second {
		t.Fatalf("timeout = (%d, %s), want 6000ms", p.config.Timeout, p.client.Timeout)
	}
	if p.config.CookieExpiresIn != 86400 {
		t.Fatalf("cookie_expires_in = %d, want 86400", p.config.CookieExpiresIn)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatalf("ssl_verify = %v, want true", p.config.SSLVerify)
	}
	if p.config.SetUserInfoHeader == nil || !*p.config.SetUserInfoHeader {
		t.Fatalf("set_userinfo_header = %v, want true", p.config.SetUserInfoHeader)
	}
}

func TestPostInitUsesConfiguredTimeoutAndSSLVerify(t *testing.T) {
	sslVerify := false
	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		Timeout:         1250,
		SSLVerify:       &sslVerify,
	})

	if p.client.Timeout != 1250*time.Millisecond {
		t.Fatalf("client timeout = %s, want 1.25s", p.client.Timeout)
	}
	transport, ok := p.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("transport TLS config = %#v, want certificate verification disabled", p.client.Transport)
	}
}

func TestHandlerRedirectsWhenNoSessionAndNoCode(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
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
	if location.Scheme != "https" || location.Host != "login.feishu.cn" || location.Path != "/oauth" {
		t.Fatalf("Location = %q, want configured OAuth endpoint", location)
	}
	if got := rr.Header().Get("Location"); got != "https://login.feishu.cn/oauth" {
		t.Fatalf("Location = %q, want configured redirect URI verbatim", got)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("redirect cookies = %#v, want none", cookies)
	}
}

func TestHandlerAcceptsQueryCodeWithoutLocalOAuthState(t *testing.T) {
	var tokenRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests++
			_, _ = w.Write([]byte(`{"access_token":"access-token-a","expires_in":7200}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou-a"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  api.URL + "/token",
		UserInfoURL:     api.URL + "/userinfo",
	})

	callback := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback?code=code-a", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })).
		ServeHTTP(rr, callback)
	if !apisixctx.IsSensitiveQueryName(callback, "code") {
		t.Fatal("feishu-auth did not register code query key")
	}

	if rr.Code != http.StatusAccepted || tokenRequests != 1 {
		t.Fatalf("callback status/token requests = %d/%d, want 202/1", rr.Code, tokenRequests)
	}
}

func TestHandlerFetchesFeishuUserInfoAndSetsSession(t *testing.T) {
	var tokenBody map[string]any
	var userinfoAuth string

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost {
				t.Fatalf("token method = %s, want POST", r.Method)
			}
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("token Content-Type = %q, want application/json", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&tokenBody); err != nil {
				t.Fatalf("decode token body: %v", err)
			}
			_, _ = w.Write([]byte(`{"access_token":"access-token-a","expires_in":7200}`))
		case "/userinfo":
			if r.Method != http.MethodGet {
				t.Fatalf("userinfo method = %s, want GET", r.Method)
			}
			userinfoAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"code":0,"data":{"open_id":"ou-a","name":"Alice"}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  api.URL + "/token",
		UserInfoURL:     api.URL + "/userinfo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.Header.Set("X-Feishu-Code", "code-a")
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("decode X-Userinfo: %v", err)
		}
		if !strings.Contains(string(decoded), `"open_id":"ou-a"`) {
			t.Fatalf("X-Userinfo = %s, want Feishu user info", decoded)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called after successful Feishu auth")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
	if tokenBody["grant_type"] != "authorization_code" ||
		tokenBody["client_id"] != "app-id" ||
		tokenBody["client_secret"] != "app-secret" ||
		tokenBody["redirect_uri"] != "https://gateway.example.com/callback" ||
		tokenBody["code"] != "code-a" {
		t.Fatalf("token body = %#v, want Feishu token exchange body", tokenBody)
	}
	if userinfoAuth != "Bearer access-token-a" {
		t.Fatalf("Authorization = %q, want Bearer access-token-a", userinfoAuth)
	}
	if cookie := findFeishuSessionCookie(rr.Result().Cookies()); cookie == nil {
		t.Fatal("feishu_session cookie was not set")
	}
}

func TestHandlerUsesExistingSessionCookie(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	})
	cookie, err := p.sessionCookie(map[string]any{"open_id": "cached-user"})
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
		if !strings.Contains(string(decoded), `"open_id":"cached-user"`) {
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
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	})
	cookie, err := p.sessionCookie(map[string]any{"open_id": "cached-user"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req = apisixctx.WithApisixVars(req, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := apisixctx.GetApisixVar(r, "$external_user").(map[string]any)
		if !ok || user["open_id"] != "cached-user" {
			t.Fatalf("$external_user = %#v, want cached Feishu user", user)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsInvalidFeishuCode(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"msg":"bad code"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  api.URL + "/token",
		UserInfoURL:     api.URL + "/userinfo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.Header.Set("X-Feishu-Code", "bad-code")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for invalid Feishu code")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid authorization code") {
		t.Fatalf("response body = %q, want invalid code message", rr.Body.String())
	}
}

func TestHandlerRejectsFailedUserInfo(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"access-token-a","expires_in":7200}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"code":99991663,"msg":"invalid token"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
		AccessTokenURL:  api.URL + "/token",
		UserInfoURL:     api.URL + "/userinfo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil)
	req.Header.Set("X-Feishu-Code", "bad-code")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when Feishu userinfo fails")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
}

func TestSessionCookieMatchesAPISIX317Defaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          "12345678",
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	})
	cookie, err := p.sessionCookie(map[string]any{"open_id": "user-a"})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	if cookie.Path != "/" || cookie.Secure || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 0 || !cookie.Expires.IsZero() {
		t.Fatalf(
			"cookie attributes = path:%q secure:%t httpOnly:%t sameSite:%v maxAge:%d expires:%v",
			cookie.Path,
			cookie.Secure,
			cookie.HttpOnly,
			cookie.SameSite,
			cookie.MaxAge,
			cookie.Expires,
		)
	}
}

func TestSessionCookieSchemaOmitsLocalCookiePolicy(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("schema JSON error = %v", err)
	}
	for _, field := range []string{"cookie_secure", "cookie_same_site"} {
		if _, ok := document.Properties[field]; ok {
			t.Fatalf("schema exposes non-APISIX field %q", field)
		}
	}
}

func TestFeishuSessionCookieUsesEncryptedConfigBoundEnvelope(t *testing.T) {
	const (
		currentSecret  = "current-session-secret"
		previousSecret = "previous-session-secret"
	)
	config := Config{
		AppID:           "app-id",
		AppSecret:       "app-secret",
		Secret:          previousSecret,
		AuthRedirectURI: "https://gateway.example.com/callback",
		RedirectURI:     "https://login.feishu.cn/oauth",
	}
	issuer := newTestPlugin(t, config)
	cookie, err := issuer.sessionCookie(map[string]any{
		"open_id": "sensitive-open-id",
		"email":   "sensitive@example.com",
	})
	if err != nil {
		t.Fatalf("sessionCookie() error = %v", err)
	}
	assertFeishuSessionCookieOpaque(t, cookie, "open_id", "sensitive-open-id", "email", "sensitive@example.com")

	t.Run("second request reuses decrypted session", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
		request.AddCookie(cookie)
		userinfo, ok := issuer.userInfoFromSession(request)
		if !ok || userinfo["open_id"] != "sensitive-open-id" || userinfo["email"] != "sensitive@example.com" {
			t.Fatalf("userInfoFromSession() = %#v/%t, want decrypted userinfo", userinfo, ok)
		}
	})

	t.Run("tamper is rejected", func(t *testing.T) {
		tampered := *cookie
		first := tampered.Value[0]
		if first == 'A' {
			first = 'B'
		} else {
			first = 'A'
		}
		tampered.Value = string(first) + tampered.Value[1:]
		assertFeishuSessionRejected(t, issuer, &tampered)
	})

	t.Run("expired envelope is rejected", func(t *testing.T) {
		payload, marshalErr := json.Marshal(sessionPayload{
			UserInfo:  map[string]any{"open_id": "not-expired-in-payload"},
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var expiredValue string
		sealErr := issuer.useSessionSecretsLocked(func(current string, _ []string) error {
			now := time.Now()
			var err error
			expiredValue, err = base.SealOAuthSession(
				payload, current, issuer.sessionFingerprint(), now.Add(-2*time.Hour), now.Add(-time.Hour),
			)
			return err
		})
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		assertFeishuSessionRejected(t, issuer, &http.Cookie{Name: sessionCookieName, Value: expiredValue})
	})

	t.Run("fallback secret opens previous session", func(t *testing.T) {
		rotatedConfig := config
		rotatedConfig.Secret = currentSecret
		rotatedConfig.SecretFallbacks = []string{previousSecret}
		rotated := newTestPlugin(t, rotatedConfig)
		request := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
		request.AddCookie(cookie)
		userinfo, ok := rotated.userInfoFromSession(request)
		if !ok || userinfo["open_id"] != "sensitive-open-id" {
			t.Fatalf("rotated userInfoFromSession() = %#v/%t, want fallback-opened userinfo", userinfo, ok)
		}
	})

	t.Run("config fingerprint mismatch is rejected", func(t *testing.T) {
		mismatchedConfig := config
		mismatchedConfig.AppID = "different-app-id"
		mismatched := newTestPlugin(t, mismatchedConfig)
		assertFeishuSessionRejected(t, mismatched, cookie)
	})

	t.Run("legacy signed plaintext cookie is rejected", func(t *testing.T) {
		payload, marshalErr := json.Marshal(sessionPayload{
			UserInfo:  map[string]any{"open_id": "legacy-user"},
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		legacy := &http.Cookie{
			Name:  sessionCookieName,
			Value: base.SignSessionValue(payload, previousSecret),
		}
		assertFeishuSessionRejected(t, issuer, legacy)
	})
}

func assertFeishuSessionCookieOpaque(t *testing.T, cookie *http.Cookie, sensitive ...string) {
	t.Helper()
	searchable := []byte(cookie.Value)
	for part := range strings.SplitSeq(cookie.Value, ".") {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err == nil {
			searchable = append(searchable, decoded...)
		}
	}
	for _, value := range sensitive {
		if strings.Contains(string(searchable), value) {
			t.Fatalf("session cookie exposes sensitive value %q", value)
		}
	}
}

func assertFeishuSessionRejected(t *testing.T, p *Plugin, cookie *http.Cookie) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil)
	request.AddCookie(cookie)
	if userinfo, ok := p.userInfoFromSession(request); ok {
		t.Fatalf("userInfoFromSession() accepted invalid cookie: %#v", userinfo)
	}
}

func findFeishuSessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == "feishu_session" {
			return cookie
		}
	}
	return nil
}
