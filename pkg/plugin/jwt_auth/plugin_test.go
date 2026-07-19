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

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/jwt-auth.db", testEvents)
		s.Start()
	})
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
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"jwt-auth": jwtConfig,
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey("jwt-auth", fmt.Sprint(jwtConfig["key"])); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for jwt-auth key %q", username, jwtConfig["key"])
}

func addConsumer(t *testing.T, username string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins":  map[string]any{},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumer(username); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not stored", username)
}

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

func TestHandlerAcceptsBearerTokenAndAttachesConsumer(t *testing.T) {
	addJWTConsumer(t, "jwt-user", "jwt-key", "jwt-secret")
	p := newTestPlugin(t, Config{})
	token := signHS256(t, "jwt-secret", map[string]any{
		"key": "jwt-key",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "jwt-user" {
			t.Fatalf("consumer_name = %v, want jwt-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := p.Handler(next)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	runnerCalled := false
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		runnerCalled = true
		next.ServeHTTP(w, r)
	})
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if !runnerCalled {
		t.Fatal("consumer plugin runner was not called")
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
	setupStore(t)
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
