package jwt_auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type jwtTestFixture struct {
	sync.Mutex
	consumers   []runtime.ConsumerRecord
	credentials []runtime.ConsumerCredentialBinding
}

var jwtTestFixtures sync.Map

func jwtFixtureFor(t *testing.T) *jwtTestFixture {
	t.Helper()
	fixture := &jwtTestFixture{}
	actual, loaded := jwtTestFixtures.LoadOrStore(t, fixture)
	if !loaded {
		t.Cleanup(func() { jwtTestFixtures.Delete(t) })
	}
	return actual.(*jwtTestFixture)
}

func addJWTConsumer(t *testing.T, username, key, secret string) {
	t.Helper()

	addJWTConsumerConfig(t, username, map[string]any{
		"key":       key,
		"secret":    secret,
		"algorithm": "HS256",
	})
}

func addJWTConsumerConfig(t *testing.T, username string, jwtConfig map[string]any) {
	t.Helper()
	fixture := jwtFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(fixture.consumers, runtime.ConsumerRecord{
		ID: username,
		Consumer: resource.Consumer{Username: username, Plugins: map[string]resource.PluginConfig{
			name: jwtConfig,
		}},
	})
	fixture.credentials = append(fixture.credentials, runtime.ConsumerCredentialBinding{
		Plugin: name, Key: fmt.Sprint(jwtConfig["key"]), ConsumerID: username,
	})
}

func addConsumer(t *testing.T, username string) {
	t.Helper()
	fixture := jwtFixtureFor(t)
	fixture.Lock()
	defer fixture.Unlock()
	fixture.consumers = append(fixture.consumers, runtime.ConsumerRecord{
		ID: username, Consumer: resource.Consumer{Username: username, Plugins: map[string]resource.PluginConfig{}},
	})
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	fixture := jwtFixtureFor(t)
	fixture.Lock()
	consumers := append([]runtime.ConsumerRecord(nil), fixture.consumers...)
	credentials := append([]runtime.ConsumerCredentialBinding(nil), fixture.credentials...)
	fixture.Unlock()
	lookup, err := runtime.NewConsumerBindings(consumers, nil, credentials)
	if err != nil {
		t.Fatalf("NewConsumerBindings() error = %v", err)
	}
	t.Cleanup(lookup.Close)

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestVerifyTokenSupportsConfiguredAlgorithms(t *testing.T) {
	for _, algorithm := range []string{
		"HS256", "HS384", "HS512",
		"RS256", "RS384", "RS512",
		"PS256", "PS384", "PS512",
		"ES256", "ES384", "ES512", "EdDSA",
	} {
		t.Run(algorithm, func(t *testing.T) {
			raw, consumer := signedTokenFixture(t, algorithm, map[string]any{
				"key": "consumer-key", "exp": time.Now().Add(time.Hour).Unix(),
			})
			claims, err := verifyToken(raw, consumer, time.Now(), 0, nil)
			if err != nil || claims["key"] != "consumer-key" {
				t.Fatalf("verifyToken() claims/error = %#v/%v", claims, err)
			}
		})
	}
}

func TestVerifyTokenRejectsAlgorithmConfusion(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "HS256", map[string]any{"key": "consumer-key"})
	confused := consumer
	confused.Algorithm = "HS512"
	if _, err := verifyToken(raw, confused, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want algorithm mismatch rejection")
	}

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	rsaConsumer := consumerConfig{
		Key:       "consumer-key",
		Algorithm: "RS256",
		PublicKey: publicKeyPEM(t, &rsaKey.PublicKey),
	}
	if _, err := verifyToken(raw, rsaConsumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want HS-token-rejected-by-RS-consumer")
	}
}

func TestVerifyTokenRejectsMalformedECDSASignature(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "ES256", map[string]any{"key": "consumer-key"})
	parts := strings.Split(raw, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	malformed := base64.RawURLEncoding.EncodeToString(signature[:len(signature)-1])
	raw = parts[0] + "." + parts[1] + "." + malformed
	if _, err := verifyToken(raw, consumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want malformed ECDSA signature rejection")
	}
}

func TestVerifyTokenExpiredWithAndWithoutLeeway(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "HS256", map[string]any{
		"key": "consumer-key", "exp": time.Now().Add(-100 * time.Second).Unix(),
	})
	if _, err := verifyToken(raw, consumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want expired token rejection")
	}
	if _, err := verifyToken(raw, consumer, time.Now(), 200*time.Second, nil); err != nil {
		t.Fatalf("verifyToken() with leeway error = %v, want acceptance", err)
	}
}

func TestVerifyTokenFutureNbfWithAndWithoutLeeway(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "HS256", map[string]any{
		"key": "consumer-key", "nbf": time.Now().Add(100 * time.Second).Unix(),
	})
	if _, err := verifyToken(raw, consumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want future nbf rejection")
	}
	if _, err := verifyToken(raw, consumer, time.Now(), 200*time.Second, nil); err != nil {
		t.Fatalf("verifyToken() with leeway error = %v, want acceptance", err)
	}
}

func TestVerifyAPISIXTimeClaimsNbfLeewayBoundary(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	leeway := 10 * time.Second
	boundary := now.Unix() + int64(leeway/time.Second)

	for _, tc := range []struct {
		name    string
		nbf     int64
		wantErr bool
	}{
		{name: "before boundary", nbf: boundary - 1},
		{name: "at boundary", nbf: boundary},
		{name: "after boundary", nbf: boundary + 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyAPISIXTimeClaims(jwt.MapClaims{"nbf": tc.nbf}, now, leeway, []string{"nbf"})
			if tc.wantErr && err == nil {
				t.Fatal("verifyAPISIXTimeClaims() error = nil, want future nbf rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("verifyAPISIXTimeClaims() error = %v, want acceptance", err)
			}
		})
	}
}

func TestVerifyTokenRejectsMissingAndInvalidClaims(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "HS256", map[string]any{"key": "consumer-key"})
	if _, err := verifyToken(raw, consumer, time.Now(), 0, []string{"exp"}); err == nil {
		t.Fatal("verifyToken() error = nil, want missing exp rejection")
	}
	if _, err := verifyToken(raw, consumer, time.Now(), 0, nil); err != nil {
		t.Fatalf("verifyToken() default error = %v, want optional exp/nbf", err)
	}

	invalid, invalidConsumer := signedTokenFixture(t, "HS256", map[string]any{
		"key": "consumer-key", "exp": "not-a-number",
	})
	if _, err := verifyToken(invalid, invalidConsumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want non-numeric exp rejection")
	}
}

func TestVerifyTokenSupportsBase64Secrets(t *testing.T) {
	secret := "raw-secret-bytes"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	base64Enabled := true
	raw := signHS256(t, secret, map[string]any{"key": "consumer-key"})
	consumer := consumerConfig{
		Key:          "consumer-key",
		Secret:       encoded,
		Algorithm:    "HS256",
		Base64Secret: &base64Enabled,
	}
	if _, err := verifyToken(raw, consumer, time.Now(), 0, nil); err != nil {
		t.Fatalf("verifyToken() base64-secret error = %v", err)
	}

	rawSecret := signHS256(t, encoded, map[string]any{"key": "consumer-key"})
	if _, err := verifyToken(rawSecret, consumer, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want base64-decoded secret mismatch")
	}
}

func TestVerifyTokenRejectsInvalidPEM(t *testing.T) {
	raw, consumer := signedTokenFixture(t, "RS256", map[string]any{"key": "consumer-key"})
	invalid := consumer
	invalid.PublicKey = "not-a-pem"
	if _, err := verifyToken(raw, invalid, time.Now(), 0, nil); err == nil {
		t.Fatal("verifyToken() error = nil, want invalid PEM rejection")
	}
}

func signedTokenFixture(t *testing.T, algorithm string, payload map[string]any) (string, consumerConfig) {
	t.Helper()

	switch {
	case strings.HasPrefix(algorithm, "HS"):
		secret := "fixture-secret-" + algorithm
		return signWithJWT(t, algorithm, []byte(secret), payload), consumerConfig{
			Key:       "consumer-key",
			Secret:    secret,
			Algorithm: algorithm,
		}
	case strings.HasPrefix(algorithm, "RS"), strings.HasPrefix(algorithm, "PS"):
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate rsa key: %v", err)
		}
		return signWithJWT(t, algorithm, key, payload), consumerConfig{
			Key:       "consumer-key",
			Algorithm: algorithm,
			PublicKey: publicKeyPEM(t, &key.PublicKey),
		}
	case strings.HasPrefix(algorithm, "ES"):
		var curve elliptic.Curve
		switch algorithm {
		case "ES256":
			curve = elliptic.P256()
		case "ES384":
			curve = elliptic.P384()
		case "ES512":
			curve = elliptic.P521()
		}
		key, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			t.Fatalf("generate ecdsa key: %v", err)
		}
		return signWithJWT(t, algorithm, key, payload), consumerConfig{
			Key:       "consumer-key",
			Algorithm: algorithm,
			PublicKey: publicKeyPEM(t, &key.PublicKey),
		}
	case algorithm == "EdDSA":
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate ed25519 key: %v", err)
		}
		return signWithJWT(t, algorithm, privateKey, payload), consumerConfig{
			Key:       "consumer-key",
			Algorithm: algorithm,
			PublicKey: publicKeyPEM(t, publicKey),
		}
	}
	t.Fatalf("unknown algorithm %s", algorithm)
	return "", consumerConfig{}
}

func signWithJWT(t *testing.T, algorithm string, key any, payload map[string]any) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.GetSigningMethod(algorithm), jwt.MapClaims(payload))
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign %s token: %v", algorithm, err)
	}
	return raw
}

func TestHandlerAcceptsBearerTokenAndAttachesConsumer(t *testing.T) {
	addJWTConsumer(t, "jwt-user", "jwt-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ctx.IsSensitiveQueryName(r, "jwt") {
			t.Fatal("jwt-auth did not register configured query key")
		}
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "jwt-user" {
			t.Fatalf("consumer_name = %v, want jwt-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := p.Handler(next)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsMissingToken(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer realm="jwt"` {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, `Bearer realm="jwt"`)
	}
	if !strings.Contains(rr.Body.String(), "Missing JWT token in request") {
		t.Fatalf("body = %q, want missing token message", rr.Body.String())
	}
}

func TestHandlerRegistersCustomQueryNameForLogging(t *testing.T) {
	p := newTestPlugin(t, Config{Query: "custom_jwt"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?custom_jwt=invalid", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid custom jwt reached downstream")
	})).ServeHTTP(rr, req)

	if !ctx.IsSensitiveQueryName(req, "custom_jwt") {
		t.Fatal("jwt-auth did not register custom query name")
	}
}

func TestHandlerUsesAnonymousConsumerWhenTokenIsMissing(t *testing.T) {
	addConsumer(t, "anonymous-jwt-user")
	p := newTestPlugin(t, Config{AnonymousConsumer: "anonymous-jwt-user"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-jwt-user" {
			t.Fatalf("consumer_name = %v, want anonymous-jwt-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRecordsMissingAnonymousConsumerProbeDiagnostic(t *testing.T) {
	p := newTestPlugin(t, Config{AnonymousConsumer: "missing-jwt-anonymous"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing jwt-auth anonymous consumer reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "failed to get anonymous consumer missing-jwt-anonymous" {
		t.Fatalf("probe diagnostics = %v, want missing-anonymous detail", diagnostics)
	}
}

func TestHandlerRejectsInvalidSignature(t *testing.T) {
	addJWTConsumer(t, "bad-signature-user", "bad-signature-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "wrong-secret", map[string]any{
		"key": "bad-signature-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), token)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "failed to verify jwt") {
		t.Fatalf("body = %q, want verification failure message", res.Body.String())
	}
}

func TestHandlerUsesAnonymousConsumerForInvalidSignatureAndHidesCredentials(t *testing.T) {
	addJWTConsumer(t, "bad-signature-user-anonymous", "bad-signature-anonymous-key", "jwt-secret")
	addConsumer(t, "anonymous-bad-signature-user")
	hideCredentials := true
	p := newTestPlugin(t, Config{
		HideCredentials:   &hideCredentials,
		AnonymousConsumer: "anonymous-bad-signature-user",
	})
	token := signHS256(t, "wrong-secret", map[string]any{
		"key": "bad-signature-anonymous-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-bad-signature-user" {
			t.Fatalf("consumer_name = %v, want anonymous-bad-signature-user", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})), token)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsExpiredTokenByDefault(t *testing.T) {
	addJWTConsumer(t, "expired-user", "expired-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "expired-key",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	res := performRequest(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})), token)

	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "failed to verify jwt") {
		t.Fatalf("body = %q, want verification failure message", res.Body.String())
	}
}

func TestHandlerAcceptsRS256TokenAndAttachesConsumer(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	addJWTConsumerConfig(t, "rsa-jwt-user", map[string]any{
		"key":        "rsa-jwt-key",
		"algorithm":  "RS256",
		"public_key": publicKeyPEM(t, &privateKey.PublicKey),
	})
	p := newTestPlugin(t, Config{})
	token := signRS256(t, privateKey, map[string]any{
		"key": "rsa-jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(assertConsumer(t, "rsa-jwt-user")), token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerAcceptsPS256TokenAndAttachesConsumer(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	addJWTConsumerConfig(t, "pss-jwt-user", map[string]any{
		"key":        "pss-jwt-key",
		"algorithm":  "PS256",
		"public_key": publicKeyPEM(t, &privateKey.PublicKey),
	})
	p := newTestPlugin(t, Config{})
	token := signPS256(t, privateKey, map[string]any{
		"key": "pss-jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(assertConsumer(t, "pss-jwt-user")), token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerAcceptsES256TokenAndAttachesConsumer(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ecdsa key: %v", err)
	}
	addJWTConsumerConfig(t, "ecdsa-jwt-user", map[string]any{
		"key":        "ecdsa-jwt-key",
		"algorithm":  "ES256",
		"public_key": publicKeyPEM(t, &privateKey.PublicKey),
	})
	p := newTestPlugin(t, Config{})
	token := signES256(t, privateKey, map[string]any{
		"key": "ecdsa-jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(assertConsumer(t, "ecdsa-jwt-user")), token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerAcceptsEdDSATokenAndAttachesConsumer(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	addJWTConsumerConfig(t, "eddsa-jwt-user", map[string]any{
		"key":        "eddsa-jwt-key",
		"algorithm":  "EdDSA",
		"public_key": publicKeyPEM(t, publicKey),
	})
	p := newTestPlugin(t, Config{})
	token := signEdDSA(t, privateKey, map[string]any{
		"key": "eddsa-jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	res := performRequest(p.Handler(assertConsumer(t, "eddsa-jwt-user")), token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestSchemaAcceptsAnonymousConsumer(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"anonymous_consumer": "anonymous-jwt-user",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected anonymous_consumer: %v", err)
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeader(t *testing.T) {
	addJWTConsumer(t, "hide-user", "hide-key", "jwt-secret")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "hide-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	res := performRequest(handler, token)
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestRemoveCookieDeletesHeaderWhenLastCredentialIsRemoved(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request.Header.Set("Cookie", "jwt=credential")

	removeCookie(request, "jwt")

	if _, present := request.Header["Cookie"]; present {
		t.Fatalf("Cookie header = %#v, want absent", request.Header["Cookie"])
	}
}

func performRequest(handler http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer "+token)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func assertConsumer(t *testing.T, username string) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != username {
			t.Fatalf("consumer_name = %v, want %s", got, username)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func signHS256(t *testing.T, secret string, payload map[string]any) string {
	t.Helper()

	unsigned := unsignedJWT(t, "HS256", payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signRS256(t *testing.T, privateKey *rsa.PrivateKey, payload map[string]any) string {
	t.Helper()

	unsigned := unsignedJWT(t, "RS256", payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign RS256 token: %v", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signPS256(t *testing.T, privateKey *rsa.PrivateKey, payload map[string]any) string {
	t.Helper()

	unsigned := unsignedJWT(t, "PS256", payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPSS(rand.Reader, privateKey, crypto.SHA256, digest[:], nil)
	if err != nil {
		t.Fatalf("sign PS256 token: %v", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signES256(t *testing.T, privateKey *ecdsa.PrivateKey, payload map[string]any) string {
	t.Helper()

	unsigned := unsignedJWT(t, "ES256", payload)
	digest := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatalf("sign ES256 token: %v", err)
	}

	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signEdDSA(t *testing.T, privateKey ed25519.PrivateKey, payload map[string]any) string {
	t.Helper()

	unsigned := unsignedJWT(t, "EdDSA", payload)
	signature := ed25519.Sign(privateKey, []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func unsignedJWT(t *testing.T, algorithm string, payload map[string]any) string {
	t.Helper()

	header := map[string]any{
		"typ": "JWT",
		"alg": algorithm,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	return fmt.Sprintf(
		"%s.%s",
		base64.RawURLEncoding.EncodeToString(headerJSON),
		base64.RawURLEncoding.EncodeToString(payloadJSON),
	)
}

func publicKeyPEM(t *testing.T, publicKey any) string {
	t.Helper()

	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}
