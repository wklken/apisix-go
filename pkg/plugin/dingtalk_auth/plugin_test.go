package dingtalk_auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if got := rr.Header().Get("Location"); got != "https://login.dingtalk.com/oauth2/auth" {
		t.Fatalf("Location = %q, want configured redirect_uri", got)
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

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code=code-a", nil)
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

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code=bad-code", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for invalid DingTalk code")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "Invalid authorization code") {
		t.Fatalf("response body = %q, want invalid code message", rr.Body.String())
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
		req := httptest.NewRequest(http.MethodGet, "http://gateway.example.com/orders?code="+code, nil)
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

func findDingTalkSessionCookie(cookies []*http.Cookie) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == "dingtalk_session" {
			return cookie
		}
	}
	return nil
}
