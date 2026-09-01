package openid_connect

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"hash"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestAPISIX317SchemaAcceptsOfficialOpenIDConnectFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "iat slack", field: "iat_slack", value: 30},
		{name: "accept none algorithm", field: "accept_none_alg", value: true},
		{name: "accept unsupported algorithm", field: "accept_unsupported_alg", value: false},
		{name: "nonce", field: "use_nonce", value: true},
		{name: "JWK expiry", field: "jwk_expires_in", value: 60},
		{name: "JWT verification cache bypass", field: "jwt_verification_cache_ignore", value: true},
		{name: "cache segment", field: "cache_segment", value: "tenant-a"},
		{name: "introspection interval", field: "introspection_interval", value: 30},
		{name: "introspection expiry claim", field: "introspection_expiry_claim", value: "expires_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"client_id": "apisix",
				"discovery": "https://idp.example.test/.well-known/openid-configuration",
				test.field:  test.value,
			}
			if err := util.Validate(config, p.GetSchema()); err != nil {
				t.Fatalf("Validate() rejected APISIX 3.17 field %q: %v", test.field, err)
			}
		})
	}
}

func TestAPISIX317SchemaDoesNotAddLocalOIDCRestrictions(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, config := range []map[string]any{
		{
			"client_id": "apisix", "discovery": "https://idp.example.test/.well-known/openid-configuration",
			"session": map[string]any{"idling_timeout": 0, "rolling_timeout": -1, "absolute_timeout": 0},
		},
		{
			"client_id": "apisix", "discovery": "https://idp.example.test/.well-known/openid-configuration",
			"session": map[string]any{"cookie_same_site": "None"},
		},
		{
			"client_id": "apisix", "discovery": "https://idp.example.test/.well-known/openid-configuration",
			"token_signing_alg_values_expected": "HS256",
		},
	} {
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("Validate() added a restriction absent from APISIX 3.17: %v", err)
		}
	}
}

func TestAPISIX317SchemaRejectsEmptyIntrospectionAddonHeaders(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"client_id": "apisix", "discovery": "https://idp.example.test/.well-known/openid-configuration",
		"introspection_addon_headers": []any{},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Validate() error = nil, want APISIX 3.17 minItems rejection")
	}
}

func TestAPISIX317NonceRoundTripsAndRejectsMismatch(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	var idToken string
	idp := newCodeFlowIDP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token",
			"id_token":     idToken,
		})
	})
	t.Cleanup(idp.Close)

	p := newTestPlugin(t, Config{
		ClientID:     "apisix",
		ClientSecret: "secret-a",
		Discovery:    idp.URL + "/.well-known/openid-configuration",
		Session:      SessionConfig{Secret: "0123456789abcdef"},
		PublicKey:    publicKeyPEM(t, &privateKey.PublicKey),
		UseNonce:     true,
	})

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called during authorization")
	})).ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil))
	authorizationURL, err := url.Parse(initial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse authorization redirect: %v", err)
	}
	nonce := authorizationURL.Query().Get("nonce")
	if nonce == "" {
		t.Fatal("authorization redirect has no nonce")
	}

	storedRequest := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	storedRequest.AddCookie(initial.Result().Cookies()[0])
	stored, err := p.readSession(storedRequest)
	if err != nil {
		t.Fatalf("readSession() error = %v", err)
	}
	if stored == nil {
		t.Fatal("stored session is nil")
	}
	if stored.Nonce != nonce {
		t.Fatalf("stored nonce = %q, want authorization nonce %q", stored.Nonce, nonce)
	}

	idToken = signRS256(t, privateKey, map[string]any{
		"iss":   idp.URL,
		"aud":   "apisix",
		"sub":   "alice",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
		"nonce": nonce,
	})
	if err := p.verifyPresentIDTokenWithNonce(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		idToken,
		nonce,
	); err != nil {
		t.Fatalf("verifyPresentIDTokenWithNonce() error = %v, want matching nonce accepted", err)
	}

	idToken = signRS256(t, privateKey, map[string]any{
		"iss":   idp.URL,
		"aud":   "apisix",
		"sub":   "alice",
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
		"nonce": "wrong-nonce",
	})
	callback := httptest.NewRequest(
		http.MethodGet,
		"https://example.com/orders/.apisix/redirect?code=code-a&state="+url.QueryEscape(
			authorizationURL.Query().Get("state"),
		),
		nil,
	)
	callback.AddCookie(initial.Result().Cookies()[0])
	callbackRecorder := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called for nonce mismatch")
	})).ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("callback status = %d, want 401; body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
}

func TestAPISIX317IATSlackAppliesToIDTokens(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	now := time.Now().Truncate(time.Second)
	tests := []struct {
		name    string
		claims  map[string]any
		wantErr bool
	}{
		{
			name: "iat beyond slack",
			claims: map[string]any{
				"iss": "https://issuer.example",
				"aud": "apisix",
				"sub": "alice",
				"iat": now.Add(31 * time.Second).Unix(),
				"exp": now.Add(time.Hour).Unix(),
			},
			wantErr: true,
		},
		{
			name: "expiry within slack",
			claims: map[string]any{
				"iss": "https://issuer.example",
				"aud": "apisix",
				"sub": "alice",
				"iat": now.Unix(),
				"exp": now.Add(-29 * time.Second).Unix(),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				ClientID:     "apisix",
				ClientSecret: "secret-a",
				Discovery:    "https://issuer.example/.well-known/openid-configuration",
				BearerOnly:   true,
				PublicKey:    publicKeyPEM(t, &privateKey.PublicKey),
				IATSlack:     new(30),
				ClaimValidator: map[string]any{
					"issuer": map[string]any{"valid_issuers": []any{"https://issuer.example"}},
				},
			})
			p.discovery = discoveryData{IDTokenSigningAlgValuesSupported: []string{"RS256"}}
			p.discoveryLoaded = true
			p.now = func() time.Time { return now }
			token := signRS256(t, privateKey, test.claims)
			err := p.verifyPresentIDToken(httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil), token)
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyPresentIDToken() error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestAPISIX317AcceptNoneAlgorithmForIDToken(t *testing.T) {
	var idp *httptest.Server
	idp = newCodeFlowIDP(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token",
			"id_token": unsignedJWT(map[string]any{
				"iss": idp.URL,
				"aud": "apisix",
				"sub": "alice",
				"iat": time.Now().Unix(),
				"exp": time.Now().Add(time.Hour).Unix(),
			}),
		})
	})
	t.Cleanup(idp.Close)

	cfg := codeFlowConfig(idp.URL)
	cfg.AcceptNoneAlgorithm = true
	p := newTestPlugin(t, cfg)

	initial := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called during authorization")
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
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called during callback")
	})).ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
}

func TestAPISIX317AcceptUnsupportedAlgorithmOnlySkipsUnsupportedSignature(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	p := newTestPlugin(t, Config{
		ClientID:                   "apisix",
		ClientSecret:               "secret-a",
		Discovery:                  "https://issuer.example/.well-known/openid-configuration",
		BearerOnly:                 true,
		PublicKey:                  publicKeyPEM(t, &privateKey.PublicKey),
		AcceptUnsupportedAlgorithm: new(true),
		ClaimValidator: map[string]any{
			"issuer": map[string]any{"valid_issuers": []any{"https://issuer.example"}},
		},
	})
	p.discovery = discoveryData{IDTokenSigningAlgValuesSupported: []string{
		"RS256", "ES256", "PS256", "EdDSA", "ES256K",
	}}
	p.discoveryLoaded = true
	claims := map[string]any{
		"iss": "https://issuer.example", "aud": "apisix", "sub": "alice",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for _, algorithm := range []string{"ES256", "PS256", "EdDSA", "ES256K"} {
		unsupported := jwtWithHeader(
			map[string]any{"alg": algorithm},
			claims,
			base64.RawURLEncoding.EncodeToString([]byte("not-a-real-signature")),
		)
		if err := p.verifyPresentIDToken(
			httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
			unsupported,
		); err != nil {
			t.Fatalf("verifyPresentIDToken(%s) error = %v, want unsupported signature ignored", algorithm, err)
		}
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if err := p.verifyPresentIDToken(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		signRS256(t, wrongKey, claims),
	); err == nil {
		t.Fatal("verifyPresentIDToken() error = nil for supported algorithm with bad signature")
	}

	disabledConfig := Config{
		ClientID:                   "apisix",
		ClientSecret:               "secret-a",
		Discovery:                  "https://issuer.example/.well-known/openid-configuration",
		BearerOnly:                 true,
		PublicKey:                  publicKeyPEM(t, &privateKey.PublicKey),
		AcceptUnsupportedAlgorithm: new(false),
		ClaimValidator: map[string]any{
			"issuer": map[string]any{"valid_issuers": []any{"https://issuer.example"}},
		},
	}
	disabled := newTestPlugin(t, disabledConfig)
	disabled.discovery = discoveryData{IDTokenSigningAlgValuesSupported: []string{"ES256K"}}
	disabled.discoveryLoaded = true
	unsupported := jwtWithHeader(
		map[string]any{"alg": "ES256K"},
		claims,
		base64.RawURLEncoding.EncodeToString([]byte("not-a-real-signature")),
	)
	if err := disabled.verifyPresentIDToken(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		unsupported,
	); err == nil {
		t.Fatal("verifyPresentIDToken() error = nil when unsupported algorithm acceptance is disabled")
	}
}

func TestAPISIX317HMACIDTokensUseClientSecret(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	p := newTestPlugin(t, Config{
		ClientID:     "apisix",
		ClientSecret: "secret-a",
		Discovery:    "https://issuer.example/.well-known/openid-configuration",
		BearerOnly:   true,
		PublicKey:    publicKeyPEM(t, &privateKey.PublicKey),
		ClaimValidator: map[string]any{
			"issuer": map[string]any{"valid_issuers": []any{"https://issuer.example"}},
		},
	})
	p.discovery = discoveryData{IDTokenSigningAlgValuesSupported: []string{"HS256", "HS384", "HS512"}}
	p.discoveryLoaded = true
	claims := map[string]any{
		"iss": "https://issuer.example", "aud": "apisix", "sub": "alice",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for _, algorithm := range []string{"HS256", "HS384", "HS512"} {
		request := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
		if err := p.verifyPresentIDToken(request, signHMACJWT(claims, algorithm, "secret-a")); err != nil {
			t.Fatalf("verifyPresentIDToken(valid %s) error = %v", algorithm, err)
		}
		if err := p.verifyPresentIDToken(request, signHMACJWT(claims, algorithm, "forged-secret")); err == nil {
			t.Fatalf("verifyPresentIDToken(forged %s) error = nil", algorithm)
		}
	}
}

func TestAPISIX317IDTokenRejectsAlgorithmMissingFromDiscovery(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.URL,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(idp.Close)
	p := newTestPlugin(t, Config{
		ClientID: "apisix", ClientSecret: "secret-a",
		Discovery: idp.URL + "/.well-known/openid-configuration", BearerOnly: true,
		PublicKey: publicKeyPEM(t, &privateKey.PublicKey),
		ClaimValidator: map[string]any{
			"issuer": map[string]any{"valid_issuers": []any{idp.URL}},
		},
	})
	claims := map[string]any{
		"iss": idp.URL, "aud": "apisix", "sub": "alice",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if err := p.verifyPresentIDToken(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		signHMACJWT(claims, "HS256", "secret-a"),
	); err == nil {
		t.Fatal("verifyPresentIDToken(unexpected HS256) error = nil")
	}
}

func TestAPISIX317CacheSegmentAndVerificationCacheIgnore(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	baseConfig := Config{
		ClientID:     "apisix",
		ClientSecret: "secret-a",
		Discovery:    "https://issuer.example/.well-known/openid-configuration",
		BearerOnly:   true,
		PublicKey:    publicKeyPEM(t, &privateKey.PublicKey),
		ClaimValidator: map[string]any{
			"issuer": map[string]any{"valid_issuers": []any{"https://issuer.example"}},
		},
	}
	first := newTestPlugin(t, baseConfig)
	secondConfig := baseConfig
	secondConfig.CacheSegment = "tenant-b"
	second := newTestPlugin(t, secondConfig)
	if first.jwtVerificationCacheKey("token-a") == second.jwtVerificationCacheKey("token-a") {
		t.Fatal("cache_segment did not separate JWT verification cache keys")
	}
	if first.introspectionCacheKey("https://issuer.example/introspect", "token-a") ==
		second.introspectionCacheKey("https://issuer.example/introspect", "token-a") {
		t.Fatal("cache_segment did not separate introspection cache keys")
	}

	token := signRS256(t, privateKey, map[string]any{
		"iss": "https://issuer.example", "aud": "apisix", "sub": "alice",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := first.verifyBearerJWT(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		token,
	); err != nil {
		t.Fatalf("verifyBearerJWT() error = %v", err)
	}
	if first.jwtVerificationCache.Len() == 0 {
		t.Fatal("JWT verification cache did not store a successful verification")
	}
	ignoredConfig := baseConfig
	ignoredConfig.JWTVerificationCacheIgnore = true
	ignored := newTestPlugin(t, ignoredConfig)
	if _, err := ignored.verifyBearerJWT(
		httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil),
		token,
	); err != nil {
		t.Fatalf("verifyBearerJWT() with cache ignore error = %v", err)
	}
	if ignored.jwtVerificationCache.Len() != 0 {
		t.Fatalf("JWT verification cache entries with ignore enabled = %d, want 0", ignored.jwtVerificationCache.Len())
	}
}

func TestAPISIX317JWKExpiryRefreshesRemoteKeys(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	var jwksRequests int
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": idp.URL, "jwks_uri": idp.URL + "/jwks",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/jwks":
			jwksRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWK(&privateKey.PublicKey, "kid-a")}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(idp.Close)
	p := newTestPlugin(t, Config{
		ClientID: "apisix", ClientSecret: "secret-a",
		Discovery: idp.URL + "/.well-known/openid-configuration", BearerOnly: true,
		UseJWKS: true, TokenSigningAlgValuesExpected: "RS256", JWKExpiresIn: new(1),
		JWTVerificationCacheIgnore: true,
	})
	now := time.Now().Truncate(time.Second)
	p.now = func() time.Time { return now }
	token := signRS256WithKid(t, privateKey, "kid-a", map[string]any{
		"iss": idp.URL, "aud": "apisix", "sub": "alice", "exp": now.Add(time.Hour).Unix(),
	})
	request := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	if _, err := p.verifyBearerJWT(request, token); err != nil {
		t.Fatalf("first verifyBearerJWT() error = %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := p.verifyBearerJWT(request, token); err != nil {
		t.Fatalf("second verifyBearerJWT() error = %v", err)
	}
	if jwksRequests != 2 {
		t.Fatalf("JWKS requests = %d, want refresh after jwk_expires_in", jwksRequests)
	}
}

func TestAPISIX317IntrospectionCacheUsesExpiryClaimAndInterval(t *testing.T) {
	var introspectionRequests int
	now := time.Unix(1_700_000_000, 0)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/introspect" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		introspectionRequests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": true, "sub": "alice", "expires_at": now.Add(10 * time.Second).Unix(),
		})
	}))
	t.Cleanup(idp.Close)
	p := newTestPlugin(t, Config{
		ClientID: "apisix", ClientSecret: "secret-a", Discovery: "https://issuer.example/discovery",
		IntrospectionEndpoint: idp.URL + "/introspect", BearerOnly: true,
		IntrospectionInterval: 3, IntrospectionExpiryClaim: "expires_at",
	})
	p.now = func() time.Time { return now }
	request := httptest.NewRequest(http.MethodGet, "https://example.com/orders", nil)
	for i := range 2 {
		if _, err := p.introspect(request, "token-a"); err != nil {
			t.Fatalf("introspect() call %d error = %v", i+1, err)
		}
	}
	if introspectionRequests != 1 {
		t.Fatalf("introspection requests = %d, want cached second response", introspectionRequests)
	}
	now = now.Add(4 * time.Second)
	if _, err := p.introspect(request, "token-a"); err != nil {
		t.Fatalf("introspect() after interval error = %v", err)
	}
	if introspectionRequests != 2 {
		t.Fatalf("introspection requests after interval = %d, want 2", introspectionRequests)
	}
}

func unsignedJWT(claims map[string]any) string {
	return jwtWithHeader(map[string]any{"alg": "none"}, claims, "")
}

func jwtWithHeader(header, claims map[string]any, signature string) string {
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	parts := []string{
		base64.RawURLEncoding.EncodeToString(headerJSON),
		base64.RawURLEncoding.EncodeToString(claimsJSON),
		signature,
	}
	return strings.Join(parts, ".")
}

func signHMACJWT(claims map[string]any, algorithm, secret string) string {
	var newHash func() hash.Hash
	switch algorithm {
	case "HS256":
		newHash = sha256.New
	case "HS384":
		newHash = sha512.New384
	case "HS512":
		newHash = sha512.New
	default:
		panic("unsupported test HMAC algorithm: " + algorithm)
	}
	unsigned := jwtWithHeader(map[string]any{"alg": algorithm}, claims, "")
	unsigned = strings.TrimSuffix(unsigned, ".")
	mac := hmac.New(newHash, []byte(secret))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
