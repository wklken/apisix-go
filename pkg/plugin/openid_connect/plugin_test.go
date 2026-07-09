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
