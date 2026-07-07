package openid_connect

import (
	"encoding/base64"
	"encoding/json"
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

func TestHandlerIntrospectsBearerTokenFromDiscovery(t *testing.T) {
	forms := make(chan url.Values, 1)
	authHeaders := make(chan string, 1)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 "http://" + r.Host,
				"introspection_endpoint": "http://" + r.Host + "/introspect",
			})
		case "/introspect":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			forms <- r.PostForm
			authHeaders <- r.Header.Get("Authorization")
			json.NewEncoder(w).Encode(map[string]any{
				"active": true,
				"scope":  "read write",
				"sub":    "alice",
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:       "apisix",
		ClientSecret:   "secret-a",
		Discovery:      idp.URL + "/.well-known/openid-configuration",
		BearerOnly:     true,
		RequiredScopes: []string{"read"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	req.Header.Set("X-Access-Token", "spoofed")
	req.Header.Set("X-Userinfo", "spoofed")
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Access-Token"); got != "token-a" {
			t.Fatalf("X-Access-Token = %q, want trusted token", got)
		}
		userinfo, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("X-Userinfo is not base64: %v", err)
		}
		if !strings.Contains(string(userinfo), `"sub":"alice"`) {
			t.Fatalf("X-Userinfo = %s, want introspection response", userinfo)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	form := <-forms
	if form.Get("token") != "token-a" {
		t.Fatalf("token = %q, want token-a", form.Get("token"))
	}
	if got := <-authHeaders; got != "Basic "+base64.StdEncoding.EncodeToString([]byte("apisix:secret-a")) {
		t.Fatalf("Authorization = %q, want client_secret_basic", got)
	}
}

func TestHandlerAcceptsXAccessTokenAsBearerInput(t *testing.T) {
	forms := make(chan url.Values, 1)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		forms <- r.PostForm
		json.NewEncoder(w).Encode(map[string]any{"active": true})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("X-Access-Token", "token-from-header")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Access-Token"); got != "token-from-header" {
			t.Fatalf("X-Access-Token = %q, want trusted token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if form := <-forms; form.Get("token") != "token-from-header" {
		t.Fatalf("token = %q, want X-Access-Token value", form.Get("token"))
	}
}

func TestHandlerRejectsMissingRequiredScope(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"active": true, "scope": "read"})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		RequiredScopes:        []string{"write"},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if strings.TrimSpace(rr.Body.String()) != `{"error":"required scopes write not present"}` {
		t.Fatalf("body = %q, want required scope error", rr.Body.String())
	}
}

func TestHandlerBearerOnlyRequiresToken(t *testing.T) {
	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: "http://idp.example.com/introspect",
		BearerOnly:            true,
		Realm:                 "demo",
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer realm="demo"` {
		t.Fatalf("WWW-Authenticate = %q, want bearer realm", got)
	}
}

func TestHandlerUnauthActionPassAllowsRequestWithoutToken(t *testing.T) {
	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: "http://idp.example.com/introspect",
		UnauthAction:          "pass",
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("X-Userinfo", "spoofed")
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Userinfo"); got != "" {
			t.Fatalf("X-Userinfo = %q, want cleared client-supplied output header", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}
