package openid_connect

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestHandlerVerifiesBearerJWTWithPublicKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	introspected := false
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": "http://" + r.Host,
			})
		case "/introspect":
			introspected = true
			t.Fatal("introspection endpoint should not be called")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:                      "apisix",
		Discovery:                     idp.URL + "/.well-known/openid-configuration",
		BearerOnly:                    true,
		PublicKey:                     publicKeyPEM(t, &privateKey.PublicKey),
		TokenSigningAlgValuesExpected: "RS256",
		RequiredScopes:                []string{"read"},
		IntrospectionEndpoint:         idp.URL + "/introspect",
	})
	token := signRS256(t, privateKey, map[string]any{
		"iss":   idp.URL,
		"sub":   "alice",
		"scope": "read write",
		"exp":   timeNowUnix() + 3600,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	called := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Access-Token"); got != token {
			t.Fatalf("X-Access-Token = %q, want JWT", got)
		}
		userinfo, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("X-Userinfo is not base64: %v", err)
		}
		if !strings.Contains(string(userinfo), `"sub":"alice"`) {
			t.Fatalf("X-Userinfo = %s, want JWT claims", userinfo)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if introspected {
		t.Fatal("introspection endpoint was called")
	}
	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerVerifiesBearerJWTWithJWKS(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":   "http://" + r.Host,
				"jwks_uri": "http://" + r.Host + "/jwks",
			})
		case "/jwks":
			json.NewEncoder(w).Encode(map[string]any{
				"keys": []any{rsaJWK(&privateKey.PublicKey, "kid-a")},
			})
		case "/introspect":
			t.Fatal("introspection endpoint should not be called")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:                      "apisix",
		Discovery:                     idp.URL + "/.well-known/openid-configuration",
		BearerOnly:                    true,
		UseJWKS:                       true,
		TokenSigningAlgValuesExpected: "RS256",
		RequiredScopes:                []string{"read"},
		IntrospectionEndpoint:         idp.URL + "/introspect",
	})
	token := signRS256WithKid(t, privateKey, "kid-a", map[string]any{
		"iss":   idp.URL,
		"sub":   "alice",
		"scope": "read",
		"exp":   timeNowUnix() + 3600,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
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

func TestHandlerRejectsMissingRequiredAudienceClaim(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice"})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		ClaimValidator: map[string]any{
			"audience": map[string]any{
				"required": true,
			},
		},
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
	if strings.TrimSpace(rr.Body.String()) != `{"error":"required audience claim not present"}` {
		t.Fatalf("body = %q, want missing audience error", rr.Body.String())
	}
}

func TestHandlerValidatesAudienceClaimAgainstClientID(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"active": true,
			"aud":    []string{"other-client", "apisix"},
			"sub":    "alice",
		})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		ClaimValidator: map[string]any{
			"audience": map[string]any{
				"match_with_client_id": true,
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userinfo, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("X-Userinfo is not base64: %v", err)
		}
		if !strings.Contains(string(userinfo), `"sub":"alice"`) {
			t.Fatalf("X-Userinfo = %s, want introspection response", userinfo)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMismatchedAudienceClaim(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"active": true,
			"aud":    "other-client",
			"sub":    "alice",
		})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		ClaimValidator: map[string]any{
			"audience": map[string]any{
				"match_with_client_id": true,
			},
		},
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
	if strings.TrimSpace(rr.Body.String()) != `{"error":"mismatched audience"}` {
		t.Fatalf("body = %q, want mismatched audience error", rr.Body.String())
	}
}

func TestHandlerRejectsClaimsThatDoNotMatchClaimSchema(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice"})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		ClaimSchema: map[string]any{
			"type":     "object",
			"required": []string{"tenant"},
			"properties": map[string]any{
				"tenant": map[string]any{"type": "string"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
		t.Fatalf("WWW-Authenticate = %q, want invalid_token error", got)
	}
}

func TestHandlerAllowsClaimsThatMatchClaimSchema(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"active": true,
			"sub":    "alice",
			"tenant": "t1",
		})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		IntrospectionEndpoint: idp.URL,
		BearerOnly:            true,
		ClaimSchema: map[string]any{
			"type":     "object",
			"required": []string{"tenant"},
			"properties": map[string]any{
				"tenant": map[string]any{"type": "string"},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userinfo, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Userinfo"))
		if err != nil {
			t.Fatalf("X-Userinfo is not base64: %v", err)
		}
		if !strings.Contains(string(userinfo), `"tenant":"t1"`) {
			t.Fatalf("X-Userinfo = %s, want tenant claim", userinfo)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
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
		Session:               SessionConfig{Secret: "0123456789abcdef"},
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

func TestPostInitRequiresSessionSecretForCodeFlow(t *testing.T) {
	p := &Plugin{config: Config{
		ClientID:  "apisix",
		Discovery: "http://idp.example.com/.well-known/openid-configuration",
	}}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want session.secret validation error")
	}
}

func TestHandlerRedirectsToAuthorizationEndpointWithAutoRedirectURI(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	p := newTestPlugin(t, codeFlowConfig(idp.URL))

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders?view=open", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	authorizationURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	if got, want := authorizationURL.String()[:len(idp.URL+"/authorize")], idp.URL+"/authorize"; got != want {
		t.Fatalf("authorization endpoint = %q, want %q", got, want)
	}
	if got := authorizationURL.Query().Get("redirect_uri"); got != "https://example.com/orders/.apisix/redirect" {
		t.Fatalf("redirect_uri = %q, want auto redirect URI", got)
	}
	if got := authorizationURL.Query().Get("state"); got == "" {
		t.Fatal("authorization redirect state is empty")
	}
	if got := authorizationURL.Query().Get("response_type"); got != "code" {
		t.Fatalf("response_type = %q, want code", got)
	}
	if rr.Result().Cookies()[0].Value == "" {
		t.Fatal("authorization redirect did not set a session cookie")
	}
}

func TestHandlerRedirectsToAuthorizationEndpointWithConfiguredRedirectURI(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.RedirectURI = "https://login.example.com/callback"
	p := newTestPlugin(t, cfg)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))

	authorizationURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	if got := authorizationURL.Query().Get("redirect_uri"); got != cfg.RedirectURI {
		t.Fatalf("redirect_uri = %q, want configured redirect URI %q", got, cfg.RedirectURI)
	}
}

func TestHandlerCodeFlowPKCEExchangesMatchingVerifier(t *testing.T) {
	var tokenForm url.Values
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		tokenForm = r.PostForm
		if got := r.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString(
			[]byte("apisix:secret-a"),
		) {
			t.Fatalf("Authorization = %q, want client_secret_basic", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"id_token":      "id-token",
			"refresh_token": "refresh-token",
			"expires_in":    3600,
		})
	})
	cfg := codeFlowConfig(idp.URL)
	cfg.UsePKCE = true
	p := newTestPlugin(t, cfg)

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))

	authorizationURL, err := url.Parse(initial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	if got := authorizationURL.Query().Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if got := authorizationURL.Query().Get("code_challenge"); got == "" {
		t.Fatal("code_challenge is empty")
	}

	callbackURL := "https://example.com/orders/.apisix/redirect?code=code-a&state=" + url.QueryEscape(
		authorizationURL.Query().Get("state"),
	)
	callback := httptest.NewRequest(http.MethodGet, callbackURL, nil)
	callback.AddCookie(initial.Result().Cookies()[0])
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(callbackRecorder, callback)

	if callbackRecorder.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	if got := tokenForm.Get("code"); got != "code-a" {
		t.Fatalf("token code = %q, want code-a", got)
	}
	verifier := tokenForm.Get("code_verifier")
	if verifier == "" {
		t.Fatal("token request did not include code_verifier")
	}
	challenge := sha256.Sum256([]byte(verifier))
	if got, want := authorizationURL.Query().Get("code_challenge"), base64.RawURLEncoding.EncodeToString(challenge[:]); got != want {
		t.Fatalf("code_challenge = %q, want challenge for code_verifier", got)
	}
}

func TestHandlerCodeFlowSupportsClientSecretPost(t *testing.T) {
	var tokenForm url.Values
	var authorization string
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		tokenForm = r.PostForm
		authorization = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token"})
	})
	cfg := codeFlowConfig(idp.URL)
	cfg.TokenEndpointAuthMethod = "client_secret_post"
	p := newTestPlugin(t, cfg)

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))
	authorizationURL, err := url.Parse(initial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	callback := httptest.NewRequest(
		http.MethodGet,
		"https://example.com/orders/.apisix/redirect?code=code-a&state="+url.QueryEscape(
			authorizationURL.Query().Get("state"),
		),
		nil,
	)
	callback.AddCookie(initial.Result().Cookies()[0])
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(httptest.NewRecorder(), callback)

	if got := tokenForm.Get("client_id"); got != "apisix" {
		t.Fatalf("client_id = %q, want apisix", got)
	}
	if got := tokenForm.Get("client_secret"); got != "secret-a" {
		t.Fatalf("client_secret = %q, want secret-a", got)
	}
	if authorization != "" {
		t.Fatalf("Authorization = %q, want no basic auth", authorization)
	}
}

func TestHandlerCodeFlowRejectsMismatchedStateWithoutTokenExchange(t *testing.T) {
	tokenCalls := 0
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		t.Fatal("token endpoint should not be called")
	})
	p := newTestPlugin(t, codeFlowConfig(idp.URL))

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))

	callback := httptest.NewRequest(
		http.MethodGet,
		"https://example.com/orders/.apisix/redirect?code=code-a&state=wrong",
		nil,
	)
	callback.AddCookie(initial.Result().Cookies()[0])
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(callbackRecorder, callback)

	if callbackRecorder.Code != http.StatusBadRequest {
		t.Fatalf("callback status = %d, want 400; body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	if tokenCalls != 0 {
		t.Fatalf("token endpoint calls = %d, want 0", tokenCalls)
	}
}

func TestHandlerCodeFlowCreatesEncryptedSessionAndUsesItDownstream(t *testing.T) {
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token",
			"id_token":      "id-token",
			"refresh_token": "refresh-token",
			"expires_in":    3600,
		})
	})
	p := newTestPlugin(t, codeFlowConfig(idp.URL))

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "https://example.com/orders?view=open", nil))
	authorizationURL, err := url.Parse(initial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}

	callback := httptest.NewRequest(
		http.MethodGet,
		"https://example.com/orders/.apisix/redirect?code=code-a&state="+url.QueryEscape(
			authorizationURL.Query().Get("state"),
		),
		nil,
	)
	callback.AddCookie(initial.Result().Cookies()[0])
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(callbackRecorder, callback)

	if got := callbackRecorder.Header().Get("Location"); got != "/orders?view=open" {
		t.Fatalf("callback redirect = %q, want original URI", got)
	}
	sessionCookie := callbackRecorder.Result().Cookies()[0]
	if strings.Contains(sessionCookie.Value, "access-token") {
		t.Fatalf("session cookie contains raw access token: %q", sessionCookie.Value)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	req.AddCookie(sessionCookie)
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Access-Token"); got != "access-token" {
			t.Fatalf("X-Access-Token = %q, want session access token", got)
		}
		if got := r.Header.Get("X-ID-Token"); got != "id-token" {
			t.Fatalf("X-ID-Token = %q, want session ID token", got)
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "refresh-token" {
			t.Fatalf("X-Refresh-Token = %q, want session refresh token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRestartsAuthenticationForExpiredOrTamperedSession(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.Session.AbsoluteTimeout = 1
	p := newTestPlugin(t, cfg)

	expiredSession, err := p.sealSession(
		[]byte(`{"created_at":1,"updated_at":1,"access_token":"expired","expires_at":1}`),
	)
	if err != nil {
		t.Fatalf("seal expired session: %v", err)
	}
	for _, cookie := range []*http.Cookie{
		{Name: cfg.Session.CookieName, Value: expiredSession},
		{Name: cfg.Session.CookieName, Value: expiredSession + "tampered"},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("next handler should not be called")
		})).ServeHTTP(rr, req)

		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want authentication restart 302; body=%s", rr.Code, rr.Body.String())
		}
	}
}

func TestHandlerLogoutClearsSessionAndUsesEndSessionEndpoint(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.LogoutPath = "/sign-out"
	cfg.PostLogoutRedirectURI = "https://example.com/after-logout"
	p := newTestPlugin(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/sign-out", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	logoutURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse logout redirect: %v", err)
	}
	if got, want := logoutURL.Path, "/logout"; got != want {
		t.Fatalf("logout endpoint path = %q, want %q", got, want)
	}
	if got := logoutURL.Query().Get("post_logout_redirect_uri"); got != cfg.PostLogoutRedirectURI {
		t.Fatalf("post_logout_redirect_uri = %q, want %q", got, cfg.PostLogoutRedirectURI)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("logout cookie = %#v, want cleared session cookie", cookies)
	}
}

func TestHandlerLogoutFallsBackToPostLogoutRedirectURI(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"issuer": "http://" + r.Host})
	}))
	t.Cleanup(idp.Close)
	cfg := codeFlowConfig(idp.URL)
	cfg.PostLogoutRedirectURI = "https://example.com/after-logout"
	p := newTestPlugin(t, cfg)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://example.com/logout", nil))

	if got := rr.Header().Get("Location"); got != cfg.PostLogoutRedirectURI {
		t.Fatalf("logout fallback = %q, want %q", got, cfg.PostLogoutRedirectURI)
	}
}

func TestSessionCookieHonorsConfiguredAttributesAndAbsoluteTimeout(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.Session = SessionConfig{
		Secret:          "0123456789abcdef",
		CookieName:      "oidc-session",
		CookiePath:      "/app",
		CookieDomain:    "example.com",
		CookieSecure:    true,
		CookieHTTPOnly:  boolPtr(false),
		CookieSameSite:  "Strict",
		AbsoluteTimeout: 3600,
	}
	p := newTestPlugin(t, cfg)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))

	cookie := rr.Result().Cookies()[0]
	if cookie.Name != cfg.Session.CookieName || cookie.Path != cfg.Session.CookiePath || cookie.Domain != cfg.Session.CookieDomain ||
		!cookie.Secure || cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Expires.IsZero() {
		t.Fatalf("session cookie attributes = %#v, want configured attributes and expiry", cookie)
	}
}

func TestPostInitMapsDeprecatedSessionLifetimeAndDefersRedis(t *testing.T) {
	p := newTestPlugin(t, Config{
		ClientID:  "apisix",
		Discovery: "http://idp.example.com/.well-known/openid-configuration",
		Session: SessionConfig{
			Secret: "0123456789abcdef",
			Cookie: &SessionCookieConfig{Lifetime: 60},
		},
	})
	if got := p.config.Session.AbsoluteTimeout; got != 60 {
		t.Fatalf("absolute_timeout = %d, want deprecated cookie lifetime", got)
	}

	p = &Plugin{config: Config{
		ClientID:  "apisix",
		Discovery: "http://idp.example.com/.well-known/openid-configuration",
		Session:   SessionConfig{Secret: "0123456789abcdef", Storage: "redis"},
	}}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want redis storage deferral error")
	}
}

func codeFlowConfig(discovery string) Config {
	return Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		Discovery:             discovery + "/.well-known/openid-configuration",
		Session:               SessionConfig{Secret: "0123456789abcdef"},
		SetRefreshTokenHeader: boolPtr(true),
	}
}

func newCodeFlowIDP(t *testing.T, tokenHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 "http://" + r.Host,
				"authorization_endpoint": "http://" + r.Host + "/authorize",
				"token_endpoint":         "http://" + r.Host + "/token",
				"end_session_endpoint":   "http://" + r.Host + "/logout",
			})
		case "/token":
			if tokenHandler == nil {
				t.Fatal("unexpected token request")
			}
			tokenHandler(w, r)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
}

func boolPtr(value bool) *bool {
	return &value
}

func signRS256(t *testing.T, privateKey *rsa.PrivateKey, payload map[string]any) string {
	t.Helper()
	return signRS256WithKid(t, privateKey, "", payload)
}

func signRS256WithKid(t *testing.T, privateKey *rsa.PrivateKey, kid string, payload map[string]any) string {
	t.Helper()

	header := map[string]any{
		"typ": "JWT",
		"alg": "RS256",
	}
	if kid != "" {
		header["kid"] = kid
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(payloadJSON)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign RS256 token: %v", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func publicKeyPEM(t *testing.T, publicKey any) string {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func rsaJWK(publicKey *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"use": "sig",
		"kid": kid,
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
	}
}

func timeNowUnix() int64 {
	return time.Now().Unix()
}
