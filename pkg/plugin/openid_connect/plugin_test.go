package openid_connect

import (
	"context"
	"crypto"
	"crypto/hmac"
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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 "http://" + r.Host,
				"introspection_endpoint": "http://" + r.Host + "/introspect",
			})
		case "/introspect":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			forms <- r.PostForm
			authHeaders <- r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{
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

func TestHandlerIntrospectsBearerTokenWithPrivateKeyJWT(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/introspect" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		assertion, err := parseJWT(r.PostForm.Get("client_assertion"))
		if err != nil {
			t.Fatalf("parse client assertion: %v", err)
		}
		if !verifyJWTSignature(assertion, "RS256", &privateKey.PublicKey) {
			t.Fatal("client assertion signature did not verify")
		}
		if got := assertion.payload["aud"]; got != idp.URL+"/introspect" {
			t.Fatalf("assertion aud = %v, want introspection endpoint", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice"})
	}))
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:                        "apisix",
		Discovery:                       idp.URL + "/.well-known/openid-configuration",
		IntrospectionEndpoint:           idp.URL + "/introspect",
		IntrospectionEndpointAuthMethod: "private_key_jwt",
		ClientRSAPrivateKey: string(
			pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}),
		),
		BearerOnly: true,
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestDiscoveryUsesConfiguredHTTPProxy(t *testing.T) {
	proxyRequests := make(chan struct {
		path          string
		proxyAuth     string
		requestTarget string
	}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests <- struct {
			path          string
			proxyAuth     string
			requestTarget string
		}{path: r.URL.Path, proxyAuth: r.Header.Get("Proxy-Authorization"), requestTarget: r.URL.String()}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "http://idp.example.test"})
	}))
	t.Cleanup(proxy.Close)

	p := newTestPlugin(t, Config{
		ClientID:   "apisix",
		Discovery:  "http://idp.example.test/.well-known/openid-configuration",
		BearerOnly: true,
		ProxyOpts: &ProxyOptions{
			HTTPProxy:              proxy.URL,
			HTTPProxyAuthorization: "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password")),
		},
	})
	if _, err := p.discoveryDoc(); err != nil {
		t.Fatalf("discoveryDoc() error = %v", err)
	}

	select {
	case request := <-proxyRequests:
		if request.path != "/.well-known/openid-configuration" {
			t.Fatalf("proxy request path = %q, want discovery path", request.path)
		}
		if request.requestTarget != "http://idp.example.test/.well-known/openid-configuration" {
			t.Fatalf("proxy request target = %q, want absolute discovery URL", request.requestTarget)
		}
		if want := "Basic " + base64.StdEncoding.EncodeToString(
			[]byte("proxy-user:proxy-password"),
		); request.proxyAuth != want {
			t.Fatalf("Proxy-Authorization = %q, want %q", request.proxyAuth, want)
		}
	default:
		t.Fatal("configured HTTP proxy did not receive the discovery request")
	}
}

func TestDiscoveryNoProxyBypassesConfiguredHTTPProxy(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "http://" + r.Host})
	}))
	t.Cleanup(idp.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("proxy should not receive no_proxy discovery request")
	}))
	t.Cleanup(proxy.Close)

	p := newTestPlugin(t, Config{
		ClientID:   "apisix",
		Discovery:  idp.URL + "/.well-known/openid-configuration",
		BearerOnly: true,
		ProxyOpts: &ProxyOptions{
			HTTPProxy: proxy.URL,
			NoProxy:   "127.0.0.1",
		},
	})
	if _, err := p.discoveryDoc(); err != nil {
		t.Fatalf("discoveryDoc() error = %v", err)
	}
}

func TestHandlerAcceptsXAccessTokenAsBearerInput(t *testing.T) {
	forms := make(chan url.Values, 1)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		forms <- r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true})
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
			_ = json.NewEncoder(w).Encode(map[string]any{
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
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":   "http://" + r.Host,
				"jwks_uri": "http://" + r.Host + "/jwks",
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "scope": "read"})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice"})
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "alice"})
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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
	if got, want := authorizationURL.Query().
		Get("code_challenge"),
		base64.RawURLEncoding.EncodeToString(
			challenge[:],
		); got != want {
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
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token"})
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

func TestHandlerCodeFlowSupportsPrivateKeyJWT(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	var idp *httptest.Server
	idp = newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want no basic auth", got)
		}
		if got := r.PostForm.Get("client_id"); got != "apisix" {
			t.Fatalf("client_id = %q, want apisix", got)
		}
		if got := r.PostForm.Get(
			"client_assertion_type",
		); got != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			t.Fatalf("client_assertion_type = %q, want JWT bearer type", got)
		}
		assertion, err := parseJWT(r.PostForm.Get("client_assertion"))
		if err != nil {
			t.Fatalf("parse client assertion: %v", err)
		}
		if got := assertion.header["alg"]; got != "RS256" {
			t.Fatalf("assertion alg = %v, want RS256", got)
		}
		if got := assertion.header["kid"]; got != "key-1" {
			t.Fatalf("assertion kid = %v, want key-1", got)
		}
		if !verifyJWTSignature(assertion, "RS256", &privateKey.PublicKey) {
			t.Fatal("client assertion signature did not verify")
		}
		if got := assertion.payload["iss"]; got != "apisix" {
			t.Fatalf("assertion iss = %v, want apisix", got)
		}
		if got := assertion.payload["sub"]; got != "apisix" {
			t.Fatalf("assertion sub = %v, want apisix", got)
		}
		if got := assertion.payload["aud"]; got != idp.URL+"/token" {
			t.Fatalf("assertion aud = %v, want token endpoint", got)
		}
		if got, ok := assertion.payload["jti"].(string); !ok || got == "" {
			t.Fatalf("assertion jti = %v, want a random string", assertion.payload["jti"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token"})
	})
	cfg := codeFlowConfig(idp.URL)
	cfg.ClientSecret = ""
	cfg.TokenEndpointAuthMethod = "private_key_jwt"
	cfg.ClientRSAPrivateKey = string(
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}),
	)
	cfg.ClientRSAPrivateKeyID = "key-1"
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
		t.Fatal("next handler should not be called for callback")
	})).ServeHTTP(httptest.NewRecorder(), callback)
}

func TestHandlerCodeFlowSupportsClientSecretJWT(t *testing.T) {
	var idp *httptest.Server
	idp = newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want no basic auth", got)
		}
		assertion, err := parseJWT(r.PostForm.Get("client_assertion"))
		if err != nil {
			t.Fatalf("parse client assertion: %v", err)
		}
		if got := assertion.header["alg"]; got != "HS256" {
			t.Fatalf("assertion alg = %v, want HS256", got)
		}
		mac := hmac.New(sha256.New, []byte("secret-a"))
		_, _ = mac.Write([]byte(assertion.signing))
		if !hmac.Equal(assertion.signature, mac.Sum(nil)) {
			t.Fatal("client assertion signature did not verify")
		}
		if got := assertion.payload["aud"]; got != idp.URL+"/token" {
			t.Fatalf("assertion aud = %v, want token endpoint", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token"})
	})
	cfg := codeFlowConfig(idp.URL)
	cfg.TokenEndpointAuthMethod = "client_secret_jwt"
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
		t.Fatal("next handler should not be called for callback")
	})).ServeHTTP(httptest.NewRecorder(), callback)
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
		_ = json.NewEncoder(w).Encode(map[string]any{
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

func TestHandlerRejectsSessionClaimsThatDoNotMatchClaimSchema(t *testing.T) {
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 "http://" + r.Host,
				"authorization_endpoint": idp.URL + "/authorize",
				"token_endpoint":         idp.URL + "/token",
				"userinfo_endpoint":      idp.URL + "/userinfo",
			})
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-token", "id_token": "id-token"})
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]any{"role": "viewer"})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)

	cfg := codeFlowConfig(idp.URL)
	cfg.ClaimSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"user": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"role": map[string]any{"type": "string", "pattern": "^admin$"},
				},
				"required": []string{"role"},
			},
		},
		"required": []string{"user", "access_token", "id_token"},
	}
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
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(callbackRecorder, callback)

	if callbackRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("callback status = %d, want 401; body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	if got := callbackRecorder.Header().Get("WWW-Authenticate"); !strings.Contains(got, `error="invalid_token"`) {
		t.Fatalf("WWW-Authenticate = %q, want invalid_token", got)
	}
}

func TestHandlerForceReauthorizeAddsConfiguredAuthorizationParameters(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.ForceReauthorize = true
	cfg.AuthorizationParams = map[string]any{
		"prompt":     "login",
		"ui_locales": "en",
	}
	p := newTestPlugin(t, cfg)

	cookieRecorder := httptest.NewRecorder()
	if err := p.writeSession(cookieRecorder, sessionData{
		CreatedAt:   time.Now().Unix(),
		UpdatedAt:   time.Now().Unix(),
		AccessToken: "active-access-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	req.AddCookie(cookieRecorder.Result().Cookies()[0])
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if called {
		t.Fatal("next handler was called despite force_reauthorize")
	}
	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	authorizationURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	if got := authorizationURL.Query().Get("prompt"); got != "login" {
		t.Fatalf("prompt = %q, want login", got)
	}
	if got := authorizationURL.Query().Get("ui_locales"); got != "en" {
		t.Fatalf("ui_locales = %q, want en", got)
	}
}

func TestHandlerRenewsExpiredSessionAccessToken(t *testing.T) {
	var tokenForm url.Values
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		tokenForm = r.PostForm
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "renewed-access-token",
			"expires_in":   3600,
		})
	})
	p := newTestPlugin(t, codeFlowConfig(idp.URL))

	cookieRecorder := httptest.NewRecorder()
	if err := p.writeSession(cookieRecorder, sessionData{
		CreatedAt:    time.Now().Add(-time.Hour).Unix(),
		UpdatedAt:    time.Now().Unix(),
		AccessToken:  "expired-access-token",
		IDToken:      "id-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	req.AddCookie(cookieRecorder.Result().Cookies()[0])
	rr := httptest.NewRecorder()
	called := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if got := r.Header.Get("X-Access-Token"); got != "renewed-access-token" {
			t.Fatalf("X-Access-Token = %q, want renewed access token", got)
		}
		if got := r.Header.Get("X-Refresh-Token"); got != "refresh-token" {
			t.Fatalf("X-Refresh-Token = %q, want retained refresh token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !called {
		t.Fatal("next handler was not called after token renewal")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if got := tokenForm.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type = %q, want refresh_token", got)
	}
	if got := tokenForm.Get("refresh_token"); got != "refresh-token" {
		t.Fatalf("refresh_token = %q, want retained session refresh token", got)
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 1 ||
		cookies[0].Value == cookieRecorder.Result().Cookies()[0].Value {
		t.Fatalf("session cookie = %#v, want a renewed session cookie", cookies)
	}
}

func TestHandlerRefreshSessionIntervalSilentlyReauthenticates(t *testing.T) {
	idp := newCodeFlowIDP(t, nil)
	cfg := codeFlowConfig(idp.URL)
	cfg.RefreshSessionInterval = new(60)
	p := newTestPlugin(t, cfg)

	cookieRecorder := httptest.NewRecorder()
	if err := p.writeSession(cookieRecorder, sessionData{
		CreatedAt:         time.Now().Add(-time.Hour).Unix(),
		UpdatedAt:         time.Now().Unix(),
		LastAuthenticated: time.Now().Add(-2 * time.Minute).Unix(),
		AccessToken:       "active-access-token",
		IDToken:           "id-token",
		ExpiresAt:         time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	req.AddCookie(cookieRecorder.Result().Cookies()[0])
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called during silent reauthentication")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	authorizationURL, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	if prompt := authorizationURL.Query().Get("prompt"); prompt != "none" {
		t.Fatalf("prompt = %q, want none", prompt)
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

func TestHandlerLogoutRevokesSessionAccessAndRefreshTokens(t *testing.T) {
	type revocationRequest struct {
		form     url.Values
		username string
		password string
	}
	revocations := make(chan revocationRequest, 2)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":               "http://" + r.Host,
				"end_session_endpoint": "http://" + r.Host + "/logout",
				"revocation_endpoint":  "http://" + r.Host + "/revoke",
			})
		case "/revoke":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("ParseForm() error = %v", err)
			}
			username, password, _ := r.BasicAuth()
			revocations <- revocationRequest{form: r.PostForm, username: username, password: password}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)

	cfg := codeFlowConfig(idp.URL)
	cfg.RevokeTokensOnLogout = true
	cfg.TokenEndpointAuthMethod = "client_secret_post"
	p := newTestPlugin(t, cfg)
	cookieRecorder := httptest.NewRecorder()
	if err := p.writeSession(cookieRecorder, sessionData{
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	}); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/logout", nil)
	req.AddCookie(cookieRecorder.Result().Cookies()[0])
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	if len(revocations) != 2 {
		t.Fatalf("revocation requests = %d, want 2", len(revocations))
	}
	requests := map[string]revocationRequest{}
	for range 2 {
		request := <-revocations
		requests[request.form.Get("token_type_hint")] = request
	}
	for hint, token := range map[string]string{"refresh_token": "refresh-token", "access_token": "access-token"} {
		request, ok := requests[hint]
		if !ok {
			t.Fatalf("missing revocation request for %s", hint)
		}
		if got := request.form.Get("token"); got != token {
			t.Fatalf("%s revocation token = %q, want %q", hint, got, token)
		}
		if request.username != "" || request.password != "" {
			t.Fatalf(
				"revocation basic credentials = %q:%q, want none for client_secret_post",
				request.username,
				request.password,
			)
		}
		if got := request.form.Get("client_id"); got != "apisix" {
			t.Fatalf("revocation client_id = %q, want apisix", got)
		}
		if got := request.form.Get("client_secret"); got != "secret-a" {
			t.Fatalf("revocation client_secret = %q, want secret-a", got)
		}
	}
}

func TestHandlerLogoutFallsBackToPostLogoutRedirectURI(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": "http://" + r.Host})
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
		CookieHTTPOnly:  new(false),
		CookieSameSite:  "Strict",
		AbsoluteTimeout: 3600,
	}
	p := newTestPlugin(t, cfg)

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))

	cookie := rr.Result().Cookies()[0]
	if cookie.Name != cfg.Session.CookieName || cookie.Path != cfg.Session.CookiePath ||
		cookie.Domain != cfg.Session.CookieDomain ||
		!cookie.Secure ||
		cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode ||
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
		t.Fatal("PostInit() error = nil, want missing redis config error")
	}
}

func TestRedisSessionStoresEncryptedStateOutsideCookie(t *testing.T) {
	p := newTestPlugin(t, Config{
		ClientID:  "apisix",
		Discovery: "http://idp.example.com/.well-known/openid-configuration",
		Session: SessionConfig{
			Secret:  "0123456789abcdef",
			Storage: "redis",
			Redis:   &SessionRedisConfig{Prefix: "oidc-sessions"},
		},
	})
	store := &fakeSessionStore{values: make(map[string]string)}
	p.sessionStore = store

	writer := httptest.NewRecorder()
	if err := p.writeSession(writer, sessionData{
		CreatedAt:    time.Now().Unix(),
		UpdatedAt:    time.Now().Unix(),
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}

	cookie := writer.Result().Cookies()[0]
	if strings.Contains(cookie.Value, "access-token") {
		t.Fatalf("redis session cookie contains access token: %q", cookie.Value)
	}
	if len(store.values) != 1 {
		t.Fatalf("stored session count = %d, want 1", len(store.values))
	}
	for key, value := range store.values {
		if !strings.HasPrefix(key, "oidc-sessions:") {
			t.Fatalf("redis key = %q, want configured prefix", key)
		}
		if strings.Contains(value, "access-token") {
			t.Fatalf("stored session contains plaintext access token: %q", value)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	req.AddCookie(cookie)
	session, err := p.readSession(req)
	if err != nil {
		t.Fatalf("readSession() error = %v", err)
	}
	if session == nil || session.AccessToken != "access-token" || session.RefreshToken != "refresh-token" {
		t.Fatalf("session = %#v, want Redis-backed tokens", session)
	}
}

func TestClearRedisSessionDeletesStoredState(t *testing.T) {
	p := newTestPlugin(t, Config{
		ClientID:  "apisix",
		Discovery: "http://idp.example.com/.well-known/openid-configuration",
		Session: SessionConfig{
			Secret:  "0123456789abcdef",
			Storage: "redis",
			Redis:   &SessionRedisConfig{Prefix: "oidc-sessions"},
		},
	})
	store := &fakeSessionStore{values: make(map[string]string)}
	p.sessionStore = store

	writer := httptest.NewRecorder()
	if err := p.writeSession(
		writer,
		sessionData{CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()},
	); err != nil {
		t.Fatalf("writeSession() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://example.com/logout", nil)
	req.AddCookie(writer.Result().Cookies()[0])
	session, err := p.readSession(req)
	if err != nil {
		t.Fatalf("readSession() error = %v", err)
	}

	p.clearSession(httptest.NewRecorder(), session)
	if len(store.values) != 0 {
		t.Fatalf("stored session count = %d, want 0 after logout", len(store.values))
	}
}

type fakeSessionStore struct {
	values map[string]string
}

func (s *fakeSessionStore) Get(_ context.Context, key string) (string, error) {
	value, ok := s.values[key]
	if !ok {
		return "", errSessionNotFound
	}
	return value, nil
}

func (s *fakeSessionStore) Set(_ context.Context, key string, value string, _ time.Duration) error {
	s.values[key] = value
	return nil
}

func (s *fakeSessionStore) Delete(_ context.Context, key string) error {
	delete(s.values, key)
	return nil
}

func codeFlowConfig(discovery string) Config {
	return Config{
		ClientID:              "apisix",
		ClientSecret:          "secret-a",
		Discovery:             discovery + "/.well-known/openid-configuration",
		Session:               SessionConfig{Secret: "0123456789abcdef"},
		SetRefreshTokenHeader: new(true),
	}
}

func newCodeFlowIDP(t *testing.T, tokenHandler http.HandlerFunc) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
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
