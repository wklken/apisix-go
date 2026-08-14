package authz_casdoor

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
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
	rotatedConfig.ClientSecretFallbacks = []string{testOldClientSecret}
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
	withoutFallback := newTestPlugin(t, Config{
		EndpointAddr: casdoor.URL,
		ClientID:     "client-a",
		ClientSecret: testOldClientSecret,
		CallbackURL:  "http://gateway.example.com/callback",
	})
	protectedReq := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders/2", nil)
	protectedReq.AddCookie(rotatedCookie)
	protectedRR := httptest.NewRecorder()
	withoutFallback.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("cookie written with new primary must not open with old key")
	})).ServeHTTP(protectedRR, protectedReq)
	if protectedRR.Code != http.StatusFound {
		t.Fatalf("old-only plugin status = %d, want authorization redirect", protectedRR.Code)
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

func TestSessionCookieSecureByDefault(t *testing.T) {
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
		EndpointAddr:   "https://door.example.com",
		ClientID:       "client-a",
		ClientSecret:   testClientSecret,
		CallbackURL:    "https://gateway.example.com/callback",
		CookieSecure:   &cookieSecure,
		CookieSameSite: "Strict",
	})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders", nil))

	cookie := findSessionCookie(rr.Result().Cookies())
	if cookie == nil {
		t.Fatal("session cookie was not set")
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
		"endpoint_addr":    "https://door.example.com",
		"client_id":        "client-a",
		"client_secret":    testClientSecret,
		"callback_url":     "https://gateway.example.com/callback",
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

func TestSessionSecretsRequireCryptographicLength(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	baseConfig := map[string]any{
		"endpoint_addr": "https://door.example.com",
		"client_id":     "client-a",
		"client_secret": "0123456789abcdef0123456789abcdef",
		"callback_url":  "https://gateway.example.com/callback",
	}
	for _, test := range []struct {
		name   string
		config map[string]any
	}{
		{
			name: "weak primary",
			config: map[string]any{
				"endpoint_addr": "https://door.example.com",
				"client_id":     "client-a",
				"client_secret": "guessable",
				"callback_url":  "https://gateway.example.com/callback",
			},
		},
		{
			name: "weak fallback",
			config: map[string]any{
				"endpoint_addr":           baseConfig["endpoint_addr"],
				"client_id":               baseConfig["client_id"],
				"client_secret":           baseConfig["client_secret"],
				"client_secret_fallbacks": []any{"guessable"},
				"callback_url":            baseConfig["callback_url"],
			},
		},
	} {
		t.Run(test.name+" schema", func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatal("Validate() error = nil, want weak session secret rejection")
			}
		})
	}

	for _, test := range []struct {
		name   string
		config Config
	}{
		{
			name: "weak primary",
			config: Config{
				EndpointAddr: "https://door.example.com",
				ClientID:     "client-a",
				ClientSecret: "guessable",
				CallbackURL:  "https://gateway.example.com/callback",
			},
		},
		{
			name: "weak fallback",
			config: Config{
				EndpointAddr:          "https://door.example.com",
				ClientID:              "client-a",
				ClientSecret:          "0123456789abcdef0123456789abcdef",
				ClientSecretFallbacks: []string{"guessable"},
				CallbackURL:           "https://gateway.example.com/callback",
			},
		},
	} {
		t.Run(test.name+" PostInit", func(t *testing.T) {
			instance := &Plugin{config: test.config}
			if err := instance.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := instance.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want weak session secret rejection")
			}
		})
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
