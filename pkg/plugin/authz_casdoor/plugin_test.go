package authz_casdoor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.newState = func() string { return "state-1" }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.newState = func() string { return "state-1" }
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestUnauthenticatedRequestRedirectsToCasdoorAuthorize(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "secret-a",
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
		ClientSecret: "secret-a",
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
	if tokenForm.Get("client_secret") != "secret-a" {
		t.Fatalf("client_secret = %q, want secret-a", tokenForm.Get("client_secret"))
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

func TestCallbackRejectsInvalidState(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "secret-a",
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
		ClientSecret: "secret-a",
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
		ClientSecret: "secret-a",
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
		ClientSecret:   "secret-a",
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
		"client_secret":    "secret-a",
		"callback_url":     "https://gateway.example.com/callback",
		"cookie_same_site": "None",
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted SameSite=None without cookie_secure=true")
	}
	config["cookie_secure"] = false
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("schema accepted SameSite=None with cookie_secure=false")
	}
	config["cookie_secure"] = true
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected secure SameSite=None cookie: %v", err)
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

func TestCasdoorSessionsExpireWithoutReads(t *testing.T) {
	original := sessionCleanupInterval
	sessionCleanupInterval = 10 * time.Millisecond
	t.Cleanup(func() { sessionCleanupInterval = original })

	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "secret-a",
		CallbackURL:  "https://gateway.example.com/callback",
	})

	base := time.Date(2026, 7, 6, 1, 2, 3, 0, time.UTC)
	p.now = func() time.Time { return base }

	p.saveSession("sid-expired", sessionData{ExpiresAt: base.Add(30 * time.Second)})
	p.saveSession("sid-live", sessionData{ExpiresAt: base.Add(2 * time.Hour)})

	// Advance the clock past the first session's expiry without reading it.
	p.now = func() time.Time { return base.Add(time.Minute) }

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.sessions.Len() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.sessions.Len(); got != 1 {
		t.Fatalf("live sessions after periodic cleanup = %d, want 1", got)
	}
	if _, ok := p.sessions.Get("sid-expired"); ok {
		t.Fatal("expired session still readable after cleanup")
	}
	if _, ok := p.getSession("sid-expired"); ok {
		t.Fatal("expired session still returned by getSession")
	}
	if _, ok := p.getSession("sid-live"); !ok {
		t.Fatal("live session was removed by cleanup")
	}
}

func TestCasdoorStopJoinsCleanupAndIsIdempotent(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "secret-a",
		CallbackURL:  "https://gateway.example.com/callback",
	})
	done := p.cleanupDone

	p.Stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup goroutine did not join after Stop")
	}

	p.Stop()
	p.Stop()
}

func TestCasdoorSessionConcurrentLookupAndSave(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "https://door.example.com",
		ClientID:     "client-a",
		ClientSecret: "secret-a",
		CallbackURL:  "https://gateway.example.com/callback",
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for j := range 200 {
				id := fmt.Sprintf("sid-%d", j)
				p.saveSession(id, sessionData{
					ExpiresAt: time.Now().Add(time.Hour),
					State:     "state-" + id,
				})
				if _, ok := p.getSession(id); !ok {
					t.Errorf("concurrent lookup lost session %s", id)
				}
			}
		})
	}
	wg.Wait()
}
