package cas_auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

const testLogoutTrustedCIDR = "192.0.2.0/24"

// TestMaterializeScopedSecretsOwnsCASCookieSecret catches resolving outside
// the exact cookie.secret authority, hashing the raw reference instead of the
// resolved value, installing weak resolved values, and retaining partial
// state after a failed attempt.
func TestMaterializeScopedSecretsOwnsCASCookieSecret(t *testing.T) {
	contextualPlaintext := "contextual-cookie-secret-contextual-cookie-secret"
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext(contextualPlaintext, "cas-auth.cookie.secret")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{
			name:     "literal",
			raw:      "literal-cookie-secret-literal-cookie-secret",
			resolved: "literal-cookie-secret-literal-cookie-secret",
		},
		{name: "environment", raw: "$ENV://X", resolved: "environment-cookie-secret-environment-cookie-secret"},
		{name: "managed", raw: "$secret://v/x", resolved: "managed-cookie-secret-managed-cookie-secret"},
		{name: "contextual ciphertext", raw: contextual, resolved: contextualPlaintext},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testCASConfig(test.raw)
			capabilityValue, scope, broker, closeAttempt := newCASScopedSecretHarness(
				t, uint64(index+1), "cas-raw-form", config,
				map[string]string{test.raw: test.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			calls := broker.scopedCalls()
			if len(calls) != 1 {
				t.Fatalf("scoped calls = %#v, want one exact cookie secret", calls)
			}
			call := calls[0]
			if call.Raw != test.raw || call.Scope.Generation != scope.Generation ||
				call.Scope.Attempt != scope.Attempt || call.Scope.Domain != generation.DomainHTTP ||
				call.Scope.Plugin != name || call.Scope.Resource != scope.Resource ||
				call.Scope.Source != capability.SecretPluginConfig || call.Scope.Field != "cookie.secret" {
				t.Fatalf("scoped call = %#v, want exact cookie.secret authority", call)
			}
			if got, want := p.config.Cookie.Secret, casCookieDescriptor(test.resolved); got != want {
				t.Fatalf("cookie secret descriptor = %q, want resolved-plaintext descriptor %q", got, want)
			}
			if p.client != nil || p.opts != (sessionOptions{}) || len(p.logoutTrustedNets) != 0 {
				t.Fatal("materialization caused PostInit side effects")
			}
		})
	}

	const raw = "$ENV://CAS_COOKIE_SHORT"
	config := testCASConfig(raw)
	capabilityValue, scope, broker, closeAttempt := newCASScopedSecretHarness(
		t, 10, "cas-retry", config, map[string]string{raw: "short"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	err = base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("resolved-short materialization error = %v, want fixed redaction", err)
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "short") {
		t.Fatalf("resolved-short error leaked credential details: %v", err)
	}
	if p.config.Cookie.Secret != raw || p.client != nil || p.opts != (sessionOptions{}) ||
		len(p.logoutTrustedNets) != 0 || p.cookieSecretSet ||
		p.cookieSecret != (secret.Value{}) || p.secretsPrepared {
		t.Fatal("resolved-short failure installed config/client/session state")
	}
	broker.setValue(raw, "retry-cookie-secret-retry-cookie-secret")
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	wantDescriptor := casCookieDescriptor("retry-cookie-secret-retry-cookie-secret")
	if got, want := p.config.Cookie.Secret, wantDescriptor; got != want {
		t.Fatalf("retry descriptor = %q, want %q", got, want)
	}
	if calls := broker.scopedCalls(); len(calls) != 2 {
		t.Fatalf("failure plus retry calls = %#v, want two complete attempts", calls)
	}
}

func TestSchemaAdmitsShortCASCookieSecretReferences(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"short", "$ENV://X", "$secret://v/x"} {
		config := map[string]any{
			"idp_uri": "https://cas.example.com", "cas_callback_uri": "/callback",
			"logout_uri": "/logout", "cookie": map[string]any{"secret": raw},
		}
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("schema rejected short secret reference %q before resolution: %v", raw, err)
		}
	}
}

func TestSchemaRejectsAPISIX317InvalidCASConfigurations(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "missing cookie secret",
			config: map[string]any{
				"idp_uri": "https://cas.example.com", "cas_callback_uri": "/callback",
				"logout_uri": "/logout", "cookie": map[string]any{},
			},
		},
		{
			name: "unsupported strict SameSite",
			config: map[string]any{
				"idp_uri": "https://cas.example.com", "cas_callback_uri": "/callback",
				"logout_uri": "/logout", "cookie": map[string]any{
					"secret": strings.Repeat("s", 32), "samesite": "Strict",
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatal("schema accepted invalid APISIX 3.17 cas-auth configuration")
			}
		})
	}
}

func TestCASScopedMaterializationDecryptsContextualCookieSecret(t *testing.T) {
	const plaintext = "legacy-context-cookie-secret-legacy-context-cookie-secret"
	service := testutil.DataEncryptionService(true, []string{"0123456789abcdef"})
	raw, err := service.EncryptForContext(plaintext, "cas-auth.cookie.secret")
	if err != nil {
		t.Fatal(err)
	}
	p := &Plugin{config: testCASConfig(raw)}
	p.SetDependencies(base.Dependencies{DataEncryption: service.Resolver()})
	capabilityValue, scope, _, cleanup := newCASScopedSecretHarness(
		t, 1, "test-route", p.config, map[string]string{raw: plaintext},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if got, want := p.config.Cookie.Secret, casCookieDescriptor(plaintext); got != want {
		t.Fatalf("legacy contextual descriptor = %q, want resolved plaintext %q", got, want)
	}
	if err := p.cookieSecret.Use(func(value string) error {
		if value != plaintext {
			t.Fatalf("scoped cookie secret = %q, want resolved plaintext", value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCASConcurrentScopedMaterializationIsSingleFlight(t *testing.T) {
	const raw = "$ENV://CAS_CONCURRENT_COOKIE_SECRET"
	config := testCASConfig(raw)
	capabilityValue, scope, broker, closeAttempt := newCASScopedSecretHarness(
		t, 30, "cas-concurrent", config,
		map[string]string{raw: "concurrent-cookie-secret-concurrent-cookie-secret"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	start := make(chan struct{})
	errorsOut := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			errorsOut <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		})
	}
	close(start)
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatalf("concurrent materialization error = %v", err)
		}
	}
	if calls := broker.scopedCalls(); len(calls) != 1 {
		t.Fatalf("concurrent resolver calls = %#v, want one materialization", calls)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newCASScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.Cookie.Secret: cfg.Cookie.Secret},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func materializeCASForTest(
	t *testing.T,
	p *Plugin,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) error {
	t.Helper()
	capabilityValue, scope, _, cleanup := newCASScopedSecretHarness(
		t, revision, resourceID, config, values,
	)
	t.Cleanup(cleanup)
	return base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
}

func TestCASInitiationCookieUsesPrivateMaterializedSecret(t *testing.T) {
	const plaintext = "private-cookie-secret-private-cookie-secret"
	p := newTestPlugin(t, testCASConfig(plaintext))
	if p.config.Cookie.Secret != casCookieDescriptor(plaintext) {
		t.Fatalf("public cookie.secret = %q, want descriptor", p.config.Cookie.Secret)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for unauthenticated request")
	})).ServeHTTP(rr, req)
	cookie := findSetCookie(rr.Result().Cookies(), requestURICookie)
	if cookie == nil {
		t.Fatal("CAS_REQUEST_URI cookie was not set")
	}
	want := base.SignRawSessionValue([]byte("/orders?debug=true"), plaintext)
	if cookie.Value != want {
		t.Fatalf("initiation cookie was not signed with private resolved secret")
	}
	if descriptorSigned := base.SignRawSessionValue(
		[]byte("/orders?debug=true"), p.config.Cookie.Secret,
	); cookie.Value == descriptorSigned {
		t.Fatal("initiation cookie was signed with the public descriptor")
	}
}

func TestCASPostInitNeverResolvesCookieSecret(t *testing.T) {
	const raw = "$ENV://CAS_POST_INIT_COOKIE_SECRET"
	t.Setenv("CAS_POST_INIT_COOKIE_SECRET", "post-init-cookie-secret-post-init-cookie-secret")
	p := &Plugin{config: testCASConfig(raw)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() before secret preparation error = %v, want credential unavailable", err)
	}
	if p.config.Cookie.Secret != raw || p.client != nil || p.opts != (sessionOptions{}) ||
		len(p.logoutTrustedNets) != 0 {
		t.Fatal("PostInit resolved or installed secret-dependent state")
	}
	capabilityValue, scope, _, cleanup := newCASScopedSecretHarness(
		t, 1, "post-init", p.config, map[string]string{raw: "post-init-cookie-secret-post-init-cookie-secret"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() after materialization error = %v", err)
	}
}

func TestCASScopedGenerationsKeepCookiesAndProcessSessionsIsolated(t *testing.T) {
	const (
		secretN  = "generation-n-cookie-secret-generation-n-cookie-secret"
		secretN1 = "generation-n1-cookie-secret-generation-n1-cookie-secret"
	)
	configN := testCASConfig("$ENV://CAS_COOKIE_N")
	configN1 := testCASConfig("$secret://vault/cas-cookie/n1")
	pN, closeN := newScopedCASTestPlugin(
		t, 20, "same-route", configN, map[string]string{configN.Cookie.Secret: secretN},
	)
	defer closeN()
	pN1, closeN1 := newScopedCASTestPlugin(
		t, 21, "same-route", configN1, map[string]string{configN1.Cookie.Secret: secretN1},
	)
	defer closeN1()

	pN.lifecycleMu.RLock()
	cookieN, err := pN.signRequestURILocked("/orders/n")
	_, nAcceptsN := pN.verifyRequestURILocked(cookieN)
	pN.lifecycleMu.RUnlock()
	if err != nil || !nAcceptsN {
		t.Fatalf("N cookie roundtrip = %t/%v, want accepted", nAcceptsN, err)
	}
	pN1.lifecycleMu.RLock()
	_, n1AcceptsN := pN1.verifyRequestURILocked(cookieN)
	cookieN1, err := pN1.signRequestURILocked("/orders/n1")
	pN1.lifecycleMu.RUnlock()
	if err != nil || n1AcceptsN {
		t.Fatalf("N+1 accepted N cookie = %t or failed signing = %v", n1AcceptsN, err)
	}
	pN.lifecycleMu.RLock()
	_, nAcceptsN1 := pN.verifyRequestURILocked(cookieN1)
	pN.lifecycleMu.RUnlock()
	if nAcceptsN1 {
		t.Fatal("N accepted N+1 initiation cookie")
	}

	pN1.storeSession("ST-survives-n-retirement", "alice")
	stopN, ok := any(pN).(interface{ Stop() })
	if !ok {
		t.Fatal("cas-auth Plugin does not implement Stop")
	}
	stopN.Stop()
	if !pN1.refreshSession("ST-survives-n-retirement") {
		t.Fatal("retiring N cleared N+1 process session")
	}
	pN1.deleteSession("ST-survives-n-retirement")

	rr := httptest.NewRecorder()
	pN.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("retired N called next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("retired N status = %d, want 503", rr.Code)
	}
}

func TestCASScopedStopWaitsForInFlightCallback(t *testing.T) {
	requestEntered := make(chan struct{})
	releaseRequest := make(chan struct{})
	var enteredOnce sync.Once
	casServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enteredOnce.Do(func() { close(requestEntered) })
		<-releaseRequest
		_, _ = w.Write([]byte(
			`<cas:serviceResponse><cas:authenticationSuccess><cas:user>alice</cas:user></cas:authenticationSuccess></cas:serviceResponse>`,
		))
	}))
	defer casServer.Close()
	config := testCASConfig("$ENV://CAS_SCOPED_STOP_SECRET")
	config.IDPURI = casServer.URL
	p, closeAttempt := newScopedCASTestPlugin(
		t, 22, "cas-scoped-stop", config,
		map[string]string{config.Cookie.Secret: "scoped-stop-cookie-secret-scoped-stop-cookie-secret"},
	)
	defer closeAttempt()
	stopper := any(p).(interface{ Stop() })

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initial, httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil),
	)
	stateCookie := findSetCookie(initial.Result().Cookies(), requestURICookie)
	if stateCookie == nil {
		close(releaseRequest)
		t.Fatal("initial request did not set initiation cookie")
	}
	callbackDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/cas_callback?ticket=ST-scoped", nil)
		req.AddCookie(stateCookie)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, req)
		callbackDone <- rr
	}()
	select {
	case <-requestEntered:
	case <-time.After(time.Second):
		close(releaseRequest)
		t.Fatal("timed out waiting for scoped CAS callback")
	}
	stopDone := make(chan struct{})
	go func() { stopper.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
		close(releaseRequest)
		t.Fatal("Stop returned while scoped CAS callback was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseRequest)
	select {
	case rr := <-callbackDone:
		if rr.Code != http.StatusFound {
			t.Fatalf("scoped callback status = %d, want 302", rr.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped callback completion")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped Stop")
	}
	if !p.retired || p.client != nil || p.secretsPrepared || p.cookieSecretSet ||
		p.cookieSecret != (secret.Value{}) {
		t.Fatal("scoped Stop retained secret/client state")
	}
	p.deleteSession("ST-scoped")
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
	if err := materializeCASForTest(
		t, p, 1, "same-site-false", p.config,
		map[string]string{p.config.Cookie.Secret: p.config.Cookie.Secret},
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
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
	if err := materializeCASForTest(
		t, p2, 2, "same-site-true", p2.config,
		map[string]string{p2.config.Cookie.Secret: p2.config.Cookie.Secret},
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
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
	if err := materializeCASForTest(
		t, p, 1, "invalid-trusted-address", p.config,
		map[string]string{p.config.Cookie.Secret: p.config.Cookie.Secret},
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
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
	p.lifecycleMu.RLock()
	signed, err := p.signRequestURILocked("/foo?bar=baz")
	decoded, ok := p.verifyRequestURILocked(signed)
	p.lifecycleMu.RUnlock()
	if err != nil || !ok || decoded != "/foo?bar=baz" {
		t.Fatalf("raw session roundtrip = %q, %t, %v; want /foo?bar=baz, true", decoded, ok, err)
	}

	tampered := signed[:len(signed)-1] + "A"
	if tampered == signed {
		tampered = signed[:len(signed)-1] + "B"
	}
	p.lifecycleMu.RLock()
	_, ok = p.verifyRequestURILocked(tampered)
	p.lifecycleMu.RUnlock()
	if ok {
		t.Fatal("VerifyRawSessionValue(tampered) = true, want false")
	}
	foreign := newTestPlugin(t, Config{Cookie: CookieConfig{Secret: strings.Repeat("X", 32)}})
	foreign.lifecycleMu.RLock()
	_, ok = foreign.verifyRequestURILocked(signed)
	foreign.lifecycleMu.RUnlock()
	if ok {
		t.Fatal("VerifyRawSessionValue(wrong secret) = true, want false")
	}
	for _, malformed := range []string{"", "no-dot-here", "abc.def"} {
		p.lifecycleMu.RLock()
		_, ok := p.verifyRequestURILocked(malformed)
		p.lifecycleMu.RUnlock()
		if ok {
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

func testCASConfig(cookieSecret string) Config {
	return Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie:         CookieConfig{Secret: cookieSecret, Secure: new(false)},
	}
}

func casCookieDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return "plugin_config#sha256:" + hex.EncodeToString(digest[:])
}

type casScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type casScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	calls  []casScopedSecretCall
}

func (*casScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (*casScopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *casScopedSecretBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, casScopedSecretCall{Scope: scope, Raw: raw})
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private CAS test value")
	}
	return value, nil
}

func (*casScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *casScopedSecretBroker) scopedCalls() []casScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]casScopedSecretCall(nil), broker.calls...)
}

func (broker *casScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func newCASScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *casScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID,
		"plugins": map[string]any{name: map[string]any{
			"idp_uri": config.IDPURI, "cas_callback_uri": config.CASCallbackURI,
			"logout_uri": config.LogoutURI, "cookie": map[string]any{"secret": config.Cookie.Secret},
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
			Key: key, Disposition: generation.DispositionPublished, Code: "cas-auth-test",
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
	broker := &casScopedSecretBroker{values: values}
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
			t.Errorf("close CAS scoped attempt: %v", err)
		}
	}
}

func newScopedCASTestPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (*Plugin, func()) {
	t.Helper()
	capabilityValue, scope, _, closeAttempt := newCASScopedSecretHarness(
		t, revision, resourceID, config, values,
	)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	return p, closeAttempt
}
