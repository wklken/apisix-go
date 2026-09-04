package authz_casdoor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_rewrite"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	testClientSecret    = "secret-a-secret-a-secret-a-secret-a"
	testOldClientSecret = "old-secret-old-secret-old-secret-old-secret"
	testNewClientSecret = "new-secret-new-secret-new-secret-new-secret"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.newState = func() (string, error) { return "state-1", nil }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.newState = func() (string, error) { return "state-1", nil }
	secrets, scope, _, cleanup := newCasdoorScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.ClientSecret: cfg.ClientSecret},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitWarnsOnlyForInsecureURLs(t *testing.T) {
	tests := []struct {
		name         string
		scheme       string
		wantWarnings []string
	}{
		{
			name:   "insecure",
			scheme: "http",
			wantWarnings: []string{
				"Using authz-casdoor endpoint_addr with no TLS is a security risk",
				"Using authz-casdoor callback_url with no TLS is a security risk",
			},
		},
		{name: "secure", scheme: "https"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var warnings []string
			stop := logger.ReplaceObserver("authz-casdoor-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "authz-casdoor") {
					warnings = append(warnings, entry.Message)
				}
			})
			defer stop()

			newTestPlugin(t, Config{
				EndpointAddr: test.scheme + "://door.example.com",
				ClientID:     "client-a",
				ClientSecret: testClientSecret,
				CallbackURL:  test.scheme + "://gateway.example.com/callback",
			})

			if !reflect.DeepEqual(warnings, test.wantWarnings) {
				t.Fatalf("warnings = %#v, want %#v", warnings, test.wantWarnings)
			}
		})
	}
}

func TestMaterializeScopedSecretsOwnsCasdoorClientSecret(t *testing.T) {
	const currentValue = "current-secret-current-secret-current-secret"
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext(currentValue, "authz-casdoor.client_secret")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	config := Config{
		EndpointAddr: "https://door.example.com", ClientID: "client-a",
		ClientSecret: contextual, CallbackURL: "https://gateway.example.com/callback",
	}
	secrets, scope, broker, closeAttempt := newCasdoorScopedSecretHarness(
		t, 1, "casdoor-scoped", config, map[string]string{contextual: currentValue}, "0123456789abcdef",
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	if calls := broker.scopedCalls(); len(calls) != 0 {
		t.Fatalf("scoped calls = %#v, want contextual ciphertext resolved without broker", calls)
	}
	if got, want := p.config.ClientSecret, casdoorDescriptor(currentValue); got != want {
		t.Fatalf("client_secret descriptor = %q, want resolved-plaintext descriptor %q", got, want)
	}
	if p.client != nil {
		t.Fatal("scoped materialization constructed an HTTP client before PostInit")
	}

	const failedRaw = "$secret://vault/casdoor/client-secret?version=2"
	failedConfig := config
	failedConfig.ClientSecret = failedRaw
	failCapability, failScope, failBroker, closeFailure := newCasdoorScopedSecretHarness(
		t, 2, "casdoor-failure", failedConfig, map[string]string{failedRaw: currentValue}, "0123456789abcdef",
	)
	defer closeFailure()
	failBroker.setFailure(failedRaw)
	failed := &Plugin{config: failedConfig}
	if err := failed.Init(); err != nil {
		t.Fatal(err)
	}
	err = base.MaterializeScopedPluginSecrets(context.Background(), failScope, failCapability, failed)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("client secret materialization error = %v, want fixed redaction", err)
	}
	if strings.Contains(err.Error(), failedRaw) || strings.Contains(err.Error(), currentValue) {
		t.Fatalf("client secret error leaked secret details: %v", err)
	}
	if failed.config.ClientSecret != failedConfig.ClientSecret || failed.clientSecretSet ||
		failed.clientSecret != (secret.Value{}) || failed.secretsPrepared || failed.client != nil {
		t.Fatalf("client secret failure retained partial state: %#v", failed)
	}
	failBroker.setFailure("")
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), failScope, failCapability, failed,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if got := failBroker.scopedCalls(); len(got) != 2 {
		t.Fatalf("failure plus retry calls = %#v, want two primary-secret attempts", got)
	}
	if failed.config.ClientSecret != casdoorDescriptor(currentValue) {
		t.Fatalf("retry installed wrong descriptors: %#v", failed.config)
	}
}

func TestCasdoorScopedSecretRawFormsUseResolvedDescriptors(t *testing.T) {
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext("contextual-secret-contextual-secret", "authz-casdoor.client_secret")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := testutil.DataEncryptionService(true, []string{"fedcba9876543210"}).
		EncryptForContext("rotated-secret-rotated-secret-rotated", "authz-casdoor.client_secret")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{
			name:     "literal",
			raw:      "literal-secret-literal-secret-literal",
			resolved: "literal-secret-literal-secret-literal",
		},
		{name: "environment", raw: "$ENV://CASDOOR_CURRENT", resolved: "environment-secret-environment-secret"},
		{name: "managed", raw: "$secret://vault/casdoor/current", resolved: "managed-secret-managed-secret-managed"},
		{name: "contextual ciphertext", raw: contextual, resolved: "contextual-secret-contextual-secret"},
		{name: "rotated ciphertext", raw: rotated, resolved: "rotated-secret-rotated-secret-rotated"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				EndpointAddr: "https://door.example.com", ClientID: "client-a",
				ClientSecret: test.raw, CallbackURL: "https://gateway.example.com/callback",
			}
			secrets, scope, broker, closeAttempt := newCasdoorScopedSecretHarness(
				t, uint64(10+index), "casdoor-raw-form", config,
				map[string]string{test.raw: test.resolved},
				"0123456789abcdef", "fedcba9876543210",
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.scopedCalls()
			isReference := strings.HasPrefix(test.raw, "$secret://") ||
				strings.HasPrefix(strings.ToUpper(test.raw), "$ENV://")
			if isReference && (len(calls) != 1 || calls[0].Raw != test.raw ||
				calls[0].Scope.Field != "client_secret") {
				t.Fatalf("scoped calls = %#v, want exact current reference", calls)
			}
			if !isReference && len(calls) != 0 {
				t.Fatalf("scoped calls = %#v, want no resolver call for literal or ciphertext", calls)
			}
			if got, want := p.config.ClientSecret, casdoorDescriptor(test.resolved); got != want {
				t.Fatalf("descriptor = %q, want %q", got, want)
			}
		})
	}
}

func TestCasdoorConcurrentScopedMaterializationIsSingleflight(t *testing.T) {
	config := Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "$ENV://CASDOOR_SINGLEFLIGHT_CURRENT",
		CallbackURL:  "https://gateway.example.com/callback",
	}
	values := map[string]string{
		config.ClientSecret: "current-singleflight-current-singleflight",
	}
	secrets, scope, broker, closeAttempt := newCasdoorScopedSecretHarness(
		t, 30, "casdoor-singleflight", config, values,
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	start := make(chan struct{})
	errorsOut := make(chan error, 32)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			<-start
			errorsOut <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
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

func TestCasdoorOAuthRequestDoesNotRetainClientSecretBody(t *testing.T) {
	const (
		raw      = "$secret://vault/casdoor/retention"
		resolved = "retention-secret-retention-secret-retention-secret"
	)
	config := Config{
		EndpointAddr: "http://casdoor.invalid", ClientID: "client-a",
		ClientSecret: raw, CallbackURL: "http://gateway.example.com/callback",
	}
	p, closeAttempt := newScopedCasdoorPlugin(
		t, 60, "casdoor-retention", config, map[string]string{raw: resolved},
	)
	defer closeAttempt()
	transport := &retainingCasdoorTransport{}
	p.client = &http.Client{Transport: transport}

	p.lifecycleMu.RLock()
	accessToken, lifetime, err := p.fetchAccessTokenLocked(
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/callback", nil),
		"code-a",
	)
	p.lifecycleMu.RUnlock()
	if err != nil || accessToken != "token-a" || lifetime != 3600 {
		t.Fatalf("fetchAccessTokenLocked() = %q/%d/%v, want token response", accessToken, lifetime, err)
	}
	if transport.request == nil {
		t.Fatal("retaining transport did not observe OAuth request")
	}
	if transport.request.GetBody != nil {
		t.Fatal("retained OAuth request exposes replayable credential body")
	}
	body, readErr := io.ReadAll(transport.request.Body)
	if readErr != nil {
		t.Fatalf("read retained OAuth request body: %v", readErr)
	}
	if len(body) != 0 || strings.Contains(string(body), resolved) ||
		strings.Contains(transport.request.URL.String(), resolved) ||
		strings.Contains(fmt.Sprint(transport.request.Header), resolved) {
		t.Fatalf("retained OAuth request exposes client secret: body=%q request=%#v", body, transport.request)
	}
	if p.config.ClientSecret != casdoorDescriptor(resolved) {
		t.Fatalf("public client_secret = %q, want descriptor", p.config.ClientSecret)
	}
	p.Stop()
}

func TestCasdoorCallbackAndStopKeepScopedSecretUseAttemptOwned(t *testing.T) {
	const (
		raw      = "$ENV://CASDOOR_CALLBACK_SECRET"
		resolved = "callback-secret-callback-secret-callback-secret"
	)
	requestEntered := make(chan string, 1)
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			requestEntered <- "parse-error"
			return
		}
		requestEntered <- r.PostForm.Get("client_secret")
		<-releaseRequest
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-a", "expires_in": 3600})
	}))
	defer func() {
		release()
		casdoor.Close()
	}()
	config := Config{
		EndpointAddr: casdoor.URL, ClientID: "client-a", ClientSecret: raw,
		CallbackURL: "http://gateway.example.com/callback",
	}
	p, closeAttempt := newScopedCasdoorPlugin(
		t, 50, "casdoor-callback", config, map[string]string{raw: resolved},
	)
	defer closeAttempt()

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initial,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil),
	)
	loginCookie := findSessionCookie(initial.Result().Cookies())
	if loginCookie == nil {
		t.Fatal("initial request did not set a session cookie")
	}
	callbackDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(
			http.MethodGet,
			"http://gateway.example.com/callback?code=code-a&state=state-1",
			nil,
		)
		req.AddCookie(loginCookie)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rr, req)
		callbackDone <- rr
	}()
	select {
	case got := <-requestEntered:
		if got != resolved {
			t.Fatalf("OAuth client_secret = %q, want resolved scoped value", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OAuth callback request")
	}

	stopDone := make(chan struct{}, 2)
	go func() { p.Stop(); stopDone <- struct{}{} }()
	go func() { p.Stop(); stopDone <- struct{}{} }()
	deadline := time.Now().Add(time.Second)
	for p.lifecycleMu.TryRLock() {
		p.lifecycleMu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Stop to wait on the lifecycle write gate")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopDone:
		t.Fatal("Stop returned while OAuth callback was in flight")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case rr := <-callbackDone:
		if rr.Code != http.StatusFound {
			t.Fatalf("callback status = %d, want 302", rr.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for callback completion")
	}
	for range 2 {
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent Stop")
		}
	}
	if !p.retired || p.client != nil || p.secretsPrepared || p.clientSecretSet ||
		p.clientSecret != (secret.Value{}) {
		t.Fatalf("Stop retained scoped callback state: %#v", p)
	}
}

func TestUnauthenticatedRequestRedirectsToCasdoorAuthorize(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/callback",
	})

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rr.Code)
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, "https://door.example.com/login/oauth/authorize?") {
		t.Fatalf("Location = %q, want Casdoor authorize URL", location)
	}
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	values := redirectURL.Query()
	if values.Get("response_type") != "code" {
		t.Fatalf("response_type = %q, want code", values.Get("response_type"))
	}
	if values.Get("scope") != "read" {
		t.Fatalf("scope = %q, want read", values.Get("scope"))
	}
	if values.Get("client_id") != "client-a" {
		t.Fatalf("client_id = %q, want client-a", values.Get("client_id"))
	}
	if values.Get("redirect_uri") != "https://gateway.example.com/callback" {
		t.Fatalf("redirect_uri = %q, want callback URL", values.Get("redirect_uri"))
	}
	if values.Get("state") != "state-1" {
		t.Fatalf("state = %q, want generated state", values.Get("state"))
	}
	if cookie := findSessionCookie(rr.Result().Cookies()); cookie == nil {
		t.Fatal("authz-casdoor session cookie was not set")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestRandomStateFailsClosed(t *testing.T) {
	state, err := randomState(failingReader{})
	if err == nil || state != "" {
		t.Fatalf("randomState() = %q, %v; want empty state and error", state, err)
	}
}

func TestHandlerRejectsRandomStateFailure(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/callback",
	})
	p.newState = func() (string, error) { return "", errors.New("entropy unavailable") }

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if cookie := findSessionCookie(rr.Result().Cookies()); cookie != nil {
		t.Fatalf("session cookie = %#v, want none", cookie)
	}
}

func TestCallbackFetchesAccessTokenAndRedirectsOriginalURI(t *testing.T) {
	var tokenForm url.Values
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/login/oauth/access_token" {
			t.Fatalf("Casdoor path = %q, want token endpoint", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want form", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		tokenForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-a",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(casdoor.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	})

	initReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1?debug=true", nil)
	initRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(initRR, initReq)
	sessionCookie := findSessionCookie(initRR.Result().Cookies())
	if sessionCookie == nil {
		t.Fatal("session cookie was not set")
	}

	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=state-1",
		nil,
	)
	callbackReq.AddCookie(sessionCookie)
	callbackRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for callback")
	})).ServeHTTP(callbackRR, callbackReq)
	if !apisixctx.IsSensitiveQueryName(callbackReq, "code") {
		t.Fatal("authz-casdoor did not register code query key")
	}

	if callbackRR.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", callbackRR.Code)
	}
	if callbackRR.Header().Get("Location") != "/orders/1?debug=true" {
		t.Fatalf("Location = %q, want original URI", callbackRR.Header().Get("Location"))
	}
	if tokenForm.Get("code") != "code-a" {
		t.Fatalf("code = %q, want code-a", tokenForm.Get("code"))
	}
	if tokenForm.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q, want authorization_code", tokenForm.Get("grant_type"))
	}
	if tokenForm.Get("client_id") != "client-a" {
		t.Fatalf("client_id = %q, want client-a", tokenForm.Get("client_id"))
	}
	if tokenForm.Get("client_secret") != testClientSecret {
		t.Fatalf("client_secret = %q, want configured secret", tokenForm.Get("client_secret"))
	}

	updated := findSessionCookie(callbackRR.Result().Cookies())
	if updated == nil {
		t.Fatal("updated session cookie was not set")
	}

	protectedReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/2", nil)
	protectedReq.AddCookie(updated)
	protectedRR := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(protectedRR, protectedReq)

	if !called {
		t.Fatal("next handler was not called for authenticated session")
	}
	if protectedRR.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", protectedRR.Code)
	}
}

func TestSafeOriginalURIRejectsSchemeRelativeRedirect(t *testing.T) {
	for _, test := range []struct {
		original string
		want     string
	}{
		{original: "/orders/1?debug=true", want: "/orders/1?debug=true"},
		{original: "//attacker.example/path", want: "/"},
		{original: "/\\attacker.example/path", want: "/"},
	} {
		if got := safeOriginalURI(test.original); got != test.want {
			t.Fatalf("safeOriginalURI(%q) = %q, want %q", test.original, got, test.want)
		}
	}
}

func TestCallbackWithoutSessionReturnsBareServiceUnavailable(t *testing.T) {
	logged := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if entry.Level == "ERROR" && entry.Message == "no session found" {
			logged <- entry
		}
	})
	t.Cleanup(stop)

	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/anything/callback",
	})
	transport := &retainingCasdoorTransport{}
	p.client.Transport = transport
	nextCalled := false
	request := httptest.NewRequest(
		http.MethodGet,
		"https://gateway.example.com/anything/callback?code=aaa&state=bbb",
		nil,
	)
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	})).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if got := response.Body.String(); got != "" {
		t.Fatalf("body = %q, want bare 503", got)
	}
	if got := response.Header().Get("Content-Type"); got != "" {
		t.Fatalf("Content-Type = %q, want absent", got)
	}
	if nextCalled {
		t.Fatal("callback without session reached next handler")
	}
	if transport.request != nil {
		t.Fatalf("callback without session contacted Casdoor: %#v", transport.request)
	}
	if !apisixctx.IsSensitiveQueryName(request, "code") {
		t.Fatal("callback did not register code as a sensitive query name")
	}
	select {
	case <-logged:
	default:
		t.Fatal("callback without session did not log the pinned APISIX error")
	}
}

func TestCallbackUsesOriginalRequestURIWhenProxyRewriteRunsFirst(t *testing.T) {
	tokenRequests := make(chan url.Values, 1)
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/login/oauth/access_token" {
			http.Error(w, "unexpected token path", http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid token form", http.StatusBadRequest)
			return
		}
		tokenRequests <- r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-a",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(casdoor.Close)

	authz := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "http://gateway.example.com/anything/callback",
	})
	rewrite := &proxy_rewrite.Plugin{}
	if err := rewrite.Init(); err != nil {
		t.Fatalf("proxy-rewrite Init() error = %v", err)
	}
	rewrite.Config().(*proxy_rewrite.Config).Uri = "/echo"
	if err := rewrite.PostInit(); err != nil {
		t.Fatalf("proxy-rewrite PostInit() error = %v", err)
	}

	terminalCalled := false
	handler := rewrite.Handler(authz.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		terminalCalled = true
		w.WriteHeader(http.StatusOK)
	})))
	initial := httptest.NewRecorder()
	handler.ServeHTTP(
		initial,
		httptest.NewRequest(
			http.MethodGet,
			"http://gateway.example.com/anything/d?param1=foo&param2=bar",
			nil,
		),
	)
	loginCookie := findSessionCookie(initial.Result().Cookies())
	if loginCookie == nil {
		t.Fatal("initial request did not set a session cookie")
	}

	callback := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/anything/callback?code=aaa&state=state-1",
		nil,
	)
	callback.AddCookie(loginCookie)
	callbackResponse := httptest.NewRecorder()
	handler.ServeHTTP(callbackResponse, callback)
	if callbackResponse.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302", callbackResponse.Code)
	}
	if got := callbackResponse.Header().Get("Location"); got != "/anything/d?param1=foo&param2=bar" {
		t.Fatalf("callback Location = %q, want original request URI", got)
	}
	select {
	case form := <-tokenRequests:
		if got := form.Get("code"); got != "aaa" {
			t.Fatalf("token code = %q, want aaa", got)
		}
	default:
		t.Fatal("callback did not exchange the authorization code")
	}

	authenticatedCookie := findSessionCookie(callbackResponse.Result().Cookies())
	if authenticatedCookie == nil {
		t.Fatal("callback did not set an authenticated session cookie")
	}
	authenticated := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/anything/d?param1=foo&param2=bar",
		nil,
	)
	authenticated.AddCookie(authenticatedCookie)
	authenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(authenticatedResponse, authenticated)
	if !terminalCalled || authenticatedResponse.Code != http.StatusOK {
		t.Fatalf(
			"authenticated request = called:%t status:%d, want called:true status:200",
			terminalCalled,
			authenticatedResponse.Code,
		)
	}
}

func TestCasdoorSessionSurvivesPluginReplacement(t *testing.T) {
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.PostForm.Get("client_secret"); got != testClientSecret {
			t.Fatalf("client_secret = %q, want configured secret", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-a",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(casdoor.Close)

	config := Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	}
	first := newTestPlugin(t, config)
	initRR := httptest.NewRecorder()
	first.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initRR,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil),
	)
	loginCookie := findSessionCookie(initRR.Result().Cookies())
	if loginCookie == nil {
		t.Fatal("login session cookie was not set")
	}
	if strings.Contains(loginCookie.Value, "state-1") || strings.Contains(loginCookie.Value, "/orders/1") {
		t.Fatalf("login session cookie exposes plaintext state: %q", loginCookie.Value)
	}

	second := newTestPlugin(t, config)
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=state-1",
		nil,
	)
	callbackReq.AddCookie(loginCookie)
	callbackRR := httptest.NewRecorder()
	second.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called for callback")
	})).ServeHTTP(callbackRR, callbackReq)
	if callbackRR.Code != http.StatusFound {
		t.Fatalf("callback status after plugin replacement = %d, want 302", callbackRR.Code)
	}

	authenticatedCookie := findSessionCookie(callbackRR.Result().Cookies())
	if authenticatedCookie == nil {
		t.Fatal("authenticated session cookie was not set")
	}
	third := newTestPlugin(t, config)
	protectedReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/2", nil)
	protectedReq.AddCookie(authenticatedCookie)
	protectedRR := httptest.NewRecorder()
	called := false
	third.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(protectedRR, protectedReq)
	if !called || protectedRR.Code != http.StatusNoContent {
		t.Fatalf("authenticated request after replacement = called:%t status:%d", called, protectedRR.Code)
	}
}

func TestCasdoorSessionSurvivesClientSecretRotation(t *testing.T) {
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.PostForm.Get("client_secret"); got != testNewClientSecret {
			t.Fatalf("client_secret = %q, want new configured secret", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-a",
			"expires_in":   3600,
		})
	}))
	t.Cleanup(casdoor.Close)

	oldConfig := Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testOldClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	}
	oldPlugin := newTestPlugin(t, oldConfig)
	initRR := httptest.NewRecorder()
	oldPlugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initRR,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil),
	)
	loginCookie := findSessionCookie(initRR.Result().Cookies())
	if loginCookie == nil {
		t.Fatal("login session cookie was not set")
	}

	rotatedConfig := oldConfig
	rotatedConfig.ClientSecret = testNewClientSecret
	rotatedPlugin := newTestPlugin(t, rotatedConfig)
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=state-1",
		nil,
	)
	callbackReq.AddCookie(loginCookie)
	callbackRR := httptest.NewRecorder()
	rotatedPlugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		callbackRR,
		callbackReq,
	)
	if callbackRR.Code != http.StatusFound {
		t.Fatalf("callback status after secret rotation = %d, want 302", callbackRR.Code)
	}

	rotatedCookie := findSessionCookie(callbackRR.Result().Cookies())
	if rotatedCookie == nil {
		t.Fatal("rotated session cookie was not set")
	}
	oldSecretPlugin := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testOldClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	})
	protectedReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/2", nil)
	protectedReq.AddCookie(rotatedCookie)
	protectedRR := httptest.NewRecorder()
	called := false
	oldSecretPlugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(protectedRR, protectedReq)
	if !called || protectedRR.Code != http.StatusNoContent {
		t.Fatalf(
			"old-secret plugin after rotation = called:%t status:%d, want accepted session",
			called,
			protectedRR.Code,
		)
	}
}

func TestCasdoorSessionRejectsInvalidCookieEnvelope(t *testing.T) {
	startedAt := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	config := Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/callback",
	}
	first := newTestPlugin(t, config)
	first.now = func() time.Time { return startedAt }
	initRR := httptest.NewRecorder()
	first.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initRR,
		httptest.NewRequest(http.MethodGet, "https://gateway.example.com/orders/1", nil),
	)
	loginCookie := findSessionCookie(initRR.Result().Cookies())
	if loginCookie == nil {
		t.Fatal("login session cookie was not set")
	}

	tests := []struct {
		name   string
		config Config
		at     time.Time
		cookie func(*http.Cookie) *http.Cookie
	}{
		{
			name: "config fingerprint mismatch",
			config: Config{
				EndpointAddr: "https://door.example.com",
				ClientID:     "client-a",
				ClientSecret: testClientSecret,
				CallbackURL:  "https://other-gateway.example.com/callback",
			},
			at: startedAt,
		},
		{name: "expiry boundary", config: config, at: startedAt.Add(10 * time.Minute)},
		{
			name:   "tamper",
			config: config,
			at:     startedAt,
			cookie: func(cookie *http.Cookie) *http.Cookie {
				copy := *cookie
				last := copy.Value[len(copy.Value)-1]
				if last == 'A' {
					last = 'B'
				} else {
					last = 'A'
				}
				copy.Value = copy.Value[:len(copy.Value)-1] + string(last)
				return &copy
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := newTestPlugin(t, test.config)
			plugin.now = func() time.Time { return test.at }
			cookie := loginCookie
			if test.cookie != nil {
				cookie = test.cookie(loginCookie)
			}
			callbackReq := httptest.NewRequest(
				http.MethodGet,
				"https://gateway.example.com/callback?code=code-a&state=state-1",
				nil,
			)
			callbackReq.AddCookie(cookie)
			callbackRR := httptest.NewRecorder()
			plugin.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
				callbackRR,
				callbackReq,
			)
			if callbackRR.Code != http.StatusServiceUnavailable {
				t.Fatalf("callback status = %d, want 503", callbackRR.Code)
			}
		})
	}
}

func TestCasdoorSessionRejectsOversizeTokenCookie(t *testing.T) {
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": strings.Repeat("x", 4000),
			"expires_in":   3600,
		})
	}))
	t.Cleanup(casdoor.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	})
	initRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		initRR,
		httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil),
	)
	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=state-1",
		nil,
	)
	callbackReq.AddCookie(findSessionCookie(initRR.Result().Cookies()))
	callbackRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(callbackRR, callbackReq)
	if callbackRR.Code != http.StatusInternalServerError {
		t.Fatalf("oversized token callback status = %d, want 500", callbackRR.Code)
	}
	if cookie := findSessionCookie(callbackRR.Result().Cookies()); cookie != nil {
		t.Fatalf("oversized authenticated session cookie = %#v, want none", cookie)
	}
}

func TestCallbackRejectsInvalidState(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/callback",
	})

	initReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil)
	initRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(initRR, initReq)
	sessionCookie := findSessionCookie(initRR.Result().Cookies())
	if sessionCookie == nil {
		t.Fatal("session cookie was not set")
	}

	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=wrong",
		nil,
	)
	callbackReq.AddCookie(sessionCookie)
	callbackRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(callbackRR, callbackReq)

	if callbackRR.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", callbackRR.Code)
	}
}

func TestInvalidTokenResponseReturnsServiceUnavailable(t *testing.T) {
	casdoor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-a",
			"expires_in":   0,
		})
	}))
	t.Cleanup(casdoor.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	})

	initReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/1", nil)
	initRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(initRR, initReq)
	sessionCookie := findSessionCookie(initRR.Result().Cookies())

	callbackReq := httptest.NewRequest(
		http.MethodGet,
		"http://gateway.example.com/callback?code=code-a&state=state-1",
		nil,
	)
	callbackReq.AddCookie(sessionCookie)
	callbackRR := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(callbackRR, callbackReq)

	if callbackRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", callbackRR.Code)
	}
}

func TestSessionCookieUsesRestySessionDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: testClientSecret,
		CallbackURL:  "https://gateway.example.com/callback",
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil))

	cookie := findSessionCookie(rr.Result().Cookies())
	if cookie == nil {
		t.Fatal("session cookie was not set")
	}
	if cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/" || cookie.MaxAge != 0 {
		t.Fatalf(
			"cookie attributes = secure:%t httpOnly:%t sameSite:%v path:%q maxAge:%d",
			cookie.Secure,
			cookie.HttpOnly,
			cookie.SameSite,
			cookie.Path,
			cookie.MaxAge,
		)
	}
}

func TestSchemaMatchesAPISIXPublicFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	wantFields := map[string]struct{}{
		"endpoint_addr": {}, "client_id": {}, "client_secret": {}, "callback_url": {},
	}
	if len(document.Properties) != len(wantFields) {
		t.Fatalf("schema properties = %v, want only APISIX fields", document.Properties)
	}
	for field := range wantFields {
		if _, ok := document.Properties[field]; !ok {
			t.Fatalf("schema is missing APISIX field %q", field)
		}
	}
	config := map[string]any{
		"endpoint_addr": "https://door.example.com",
		"client_id":     "client-a",
		"client_secret": "short",
		"callback_url":  "https://gateway.example.com/callback",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected APISIX-valid short client_secret: %v", err)
	}
}

func findSessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if strings.HasPrefix(cookie.Name, "authz_casdoor_session_") {
			return cookie
		}
	}
	return nil
}

func newScopedCasdoorPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (*Plugin, func()) {
	t.Helper()
	secrets, scope, _, closeAttempt := newCasdoorScopedSecretHarness(
		t, revision, resourceID, config, values,
	)
	p := &Plugin{config: config}
	p.newState = func() (string, error) { return "state-1", nil }
	if err := p.Init(); err != nil {
		closeAttempt()
		t.Fatal(err)
	}
	p.newState = func() (string, error) { return "state-1", nil }
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
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

type retainingCasdoorTransport struct {
	request *http.Request
}

func (transport *retainingCasdoorTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"token-a","expires_in":3600}`)),
		Request:    request,
	}, nil
}

func casdoorDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return "plugin_config#sha256:" + hex.EncodeToString(digest[:])
}

type casdoorScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type casdoorScopedSecretBroker struct {
	mu      sync.Mutex
	values  map[string]string
	failRaw string
	calls   []casdoorScopedSecretCall
}

func (broker *casdoorScopedSecretBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, casdoorScopedSecretCall{Scope: scope, Raw: raw})
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver failed for %s private-casdoor-session", raw)
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return "", errors.New("missing private Casdoor test value")
}

func (broker *casdoorScopedSecretBroker) scopedCalls() []casdoorScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]casdoorScopedSecretCall(nil), broker.calls...)
}

func (broker *casdoorScopedSecretBroker) setFailure(raw string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.failRaw = raw
}

func newCasdoorScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
	keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *casdoorScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID,
		"plugins": map[string]any{name: map[string]any{
			"endpoint_addr": config.EndpointAddr,
			"client_id":     config.ClientID,
			"client_secret": config.ClientSecret,
			"callback_url":  config.CallbackURL,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "authz-casdoor-test",
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
	broker := &casdoorScopedSecretBroker{values: maps.Clone(values)}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
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
			t.Errorf("close Casdoor scoped generation: %v", err)
		}
	}
}
