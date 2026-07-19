package feishu_auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
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
	if got := rr.Header().Get("Location"); got != "https://login.feishu.cn/oauth" {
		t.Fatalf("Location = %q, want configured redirect_uri", got)
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

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code=code-a", nil)
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

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code=bad-code", nil)
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

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code=bad-code", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when Feishu userinfo fails")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
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
