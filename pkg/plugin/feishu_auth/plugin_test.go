package feishu_auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
