package cas_auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

func TestUnauthenticatedRequestRedirectsToCASLogin(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: boolPtr(false),
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rr.Code)
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, "https://cas.example.com/login?") {
		t.Fatalf("Location = %q, want CAS login URL", location)
	}
	redirectURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if redirectURL.Query().Get("service") != "http://example.com/cas_callback" {
		t.Fatalf("service = %q, want callback URL", redirectURL.Query().Get("service"))
	}
	if got := findSetCookie(rr.Result().Cookies(), "CAS_REQUEST_URI"); got == nil {
		t.Fatal("CAS_REQUEST_URI cookie was not set")
	} else if got.Secure {
		t.Fatal("CAS_REQUEST_URI Secure = true, want false from config")
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
		w.Write(
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
			Secure: boolPtr(false),
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

	if callbackRR.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", callbackRR.Code)
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
			Secure: boolPtr(false),
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

func TestLogoutDeletesSessionAndRedirectsToCASLogout(t *testing.T) {
	p := newTestPlugin(t, Config{
		IDPURI:         "https://cas.example.com",
		CASCallbackURI: "/cas_callback",
		LogoutURI:      "/logout",
		Cookie: CookieConfig{
			Secret: strings.Repeat("s", 32),
			Secure: boolPtr(false),
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

	if rr.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want 307", rr.Code)
	}
	if rr.Header().Get("Location") != "https://cas.example.com/logout" {
		t.Fatalf("Location = %q, want CAS logout URL", rr.Header().Get("Location"))
	}
	deleted := findSetCookie(rr.Result().Cookies(), sessionName)
	if deleted == nil || deleted.MaxAge != -1 {
		t.Fatalf("session delete cookie = %#v, want MaxAge=-1", deleted)
	}
	if _, ok := p.sessions[p.sessionKey("ST-1")]; ok {
		t.Fatal("session still exists after logout")
	}
}

func boolPtr(v bool) *bool {
	return &v
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
