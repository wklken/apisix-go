package jwe_decrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/jwe-decrypt.db", testEvents)
		s.Start()
	})
}

func addJWEConsumer(t *testing.T, username, key, secret string, base64Encoded bool) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"jwe-decrypt": map[string]any{
				"key":               key,
				"secret":            secret,
				"is_base64_encoded": base64Encoded,
			},
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
		if _, err := store.GetConsumerByPluginKey("jwe-decrypt", key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for jwe-decrypt key %q", username, key)
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

func TestHandlerDecryptsBearerJWEAndForwardsPlaintext(t *testing.T) {
	secret := "12345678901234567890123456789012"
	addJWEConsumer(t, "jwe-user", "kid-1", secret, false)
	p := newTestPlugin(t, Config{ForwardHeader: "X-Forwarded-Authorization"})
	token := makeCompactJWE(t, "kid-1", []byte(secret), "Bearer upstream-token")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Forwarded-Authorization"); got != "Bearer upstream-token" {
			t.Fatalf("forward header = %q, want plaintext", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerDecryptsBase64EncodedConsumerSecret(t *testing.T) {
	secret := []byte("abcdefghijklmnopqrstuvwxyz123456")
	encodedSecret := base64.RawURLEncoding.EncodeToString(secret)
	addJWEConsumer(t, "jwe-base64-user", "kid-base64", encodedSecret, true)
	p := newTestPlugin(t, Config{})
	token := makeCompactJWE(t, "kid-base64", secret, "Bearer base64-secret")

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer base64-secret" {
			t.Fatalf("Authorization header = %q, want decrypted plaintext", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandlerRejectsMissingTokenWhenStrict(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "missing JWE token in request") {
		t.Fatalf("body = %q, want missing token message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidToken(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", "not-a-jwe")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "JWE token invalid") {
		t.Fatalf("body = %q, want invalid token message", rr.Body.String())
	}
}

func TestHandlerRejectsUnknownKid(t *testing.T) {
	token := makeCompactJWE(t, "unknown-kid", []byte("12345678901234567890123456789012"), "Bearer upstream-token")
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("Authorization", token)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "invalid kid in JWE token") {
		t.Fatalf("body = %q, want invalid kid message", rr.Body.String())
	}
}

func TestDecryptJWERejectsConsumerSecretThatIsNot32Bytes(t *testing.T) {
	secret := []byte("1234567890123456")
	token := makeCompactJWE(t, "kid-short-secret", secret, "Bearer upstream-token")
	parsed, err := parseCompactJWE(token)
	if err != nil {
		t.Fatalf("parseCompactJWE() error = %v", err)
	}

	_, err = decryptJWE(parsed, map[string]any{"secret": string(secret)})
	if err == nil {
		t.Fatal("decryptJWE() error = nil, want 32-byte secret validation error")
	}
}

func makeCompactJWE(t *testing.T, kid string, secret []byte, plaintext string) string {
	t.Helper()

	header, err := json.Marshal(map[string]any{
		"alg": "dir",
		"enc": "A256GCM",
		"kid": kid,
	})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	protectedHeader := base64.RawURLEncoding.EncodeToString(header)
	iv := []byte("123456789012")

	block, err := aes.NewCipher(secret)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new gcm: %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), []byte(protectedHeader))
	tagStart := len(sealed) - gcm.Overhead()

	return strings.Join([]string{
		protectedHeader,
		"",
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(sealed[:tagStart]),
		base64.RawURLEncoding.EncodeToString(sealed[tagStart:]),
	}, ".")
}
