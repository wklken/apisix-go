package base

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestOAuthSessionRoundTripUsesUniqueNonce(t *testing.T) {
	issuedAt := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	expiresAt := issuedAt.Add(time.Hour)
	payload := []byte(`{"state":"state-a"}`)

	first, err := SealOAuthSession(payload, "current-secret", "config-a", issuedAt, expiresAt)
	if err != nil {
		t.Fatalf("SealOAuthSession() error = %v", err)
	}
	second, err := SealOAuthSession(payload, "current-secret", "config-a", issuedAt, expiresAt)
	if err != nil {
		t.Fatalf("SealOAuthSession() second error = %v", err)
	}
	if first == second {
		t.Fatal("two OAuth session cookies are identical, want unique nonces")
	}

	opened, err := OpenOAuthSession(first, "current-secret", nil, "config-a", issuedAt)
	if err != nil {
		t.Fatalf("OpenOAuthSession() error = %v", err)
	}
	if got := string(opened); got != string(payload) {
		t.Fatalf("opened payload = %q, want %q", got, payload)
	}
}

func TestOAuthSessionOpensWithFallbackSecret(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	sealed, err := SealOAuthSession([]byte("payload"), "old-secret", "config-a", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SealOAuthSession() error = %v", err)
	}

	opened, err := OpenOAuthSession(sealed, "new-secret", []string{"old-secret"}, "config-a", now)
	if err != nil {
		t.Fatalf("OpenOAuthSession() with fallback error = %v", err)
	}
	if got := string(opened); got != "payload" {
		t.Fatalf("opened payload = %q, want payload", got)
	}
	if _, err := OpenOAuthSession(sealed, "new-secret", nil, "config-a", now); err == nil {
		t.Fatal("OpenOAuthSession() without fallback error = nil")
	}
}

func TestOAuthSessionRejectsInvalidEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	sealed, err := SealOAuthSession([]byte("payload"), "secret", "config-a", now, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SealOAuthSession() error = %v", err)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("decode sealed session: %v", err)
	}
	decoded[len(decoded)-1] ^= 1
	tampered := base64.RawURLEncoding.EncodeToString(decoded)

	tests := []struct {
		name        string
		value       string
		fingerprint string
		at          time.Time
	}{
		{name: "tampered", value: tampered, fingerprint: "config-a", at: now},
		{name: "wrong fingerprint", value: sealed, fingerprint: "config-b", at: now},
		{name: "expiry boundary", value: sealed, fingerprint: "config-a", at: now.Add(time.Hour)},
		{
			name:        "unknown version",
			value:       sealOAuthSessionVersionForTest(t, 2, "secret", "config-a", now),
			fingerprint: "config-a",
			at:          now,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := OpenOAuthSession(test.value, "secret", nil, test.fingerprint, test.at); err == nil {
				t.Fatal("OpenOAuthSession() error = nil")
			}
		})
	}
}

func TestOAuthSessionRejectsOversizeCookie(t *testing.T) {
	now := time.Date(2026, 8, 14, 1, 2, 3, 0, time.UTC)
	if _, err := SealOAuthSession(
		[]byte(strings.Repeat("x", 4000)),
		"secret",
		"config-a",
		now,
		now.Add(time.Hour),
	); err == nil {
		t.Fatal("SealOAuthSession() oversized payload error = nil")
	}
}

func sealOAuthSessionVersionForTest(
	t *testing.T,
	version int,
	secret string,
	fingerprint string,
	issuedAt time.Time,
) string {
	t.Helper()

	plaintext, err := json.Marshal(struct {
		Version     int    `json:"v"`
		IssuedAt    int64  `json:"iat"`
		ExpiresAt   int64  `json:"exp"`
		Fingerprint string `json:"fp"`
		Payload     []byte `json:"data"`
	}{
		Version:     version,
		IssuedAt:    issuedAt.Unix(),
		ExpiresAt:   issuedAt.Add(time.Hour).Unix(),
		Fingerprint: fingerprint,
		Payload:     []byte("payload"),
	})
	if err != nil {
		t.Fatalf("marshal session envelope: %v", err)
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("new AES cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("new GCM: %v", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed)
}

func testHMACSignature(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestSignAndVerifySessionValue(t *testing.T) {
	payload := []byte(`{"sub":"user-1"}`)
	signed := SignSessionValue(payload, "secret")

	decoded, ok := VerifySessionValue(signed, "secret", nil)
	if !ok {
		t.Fatal("VerifySessionValue() = false, want true")
	}
	if got := string(decoded); got != string(payload) {
		t.Fatalf("decoded payload = %q, want %q", got, payload)
	}
}

func TestVerifySessionValueFallbackSecret(t *testing.T) {
	payload := []byte(`{"sub":"user-1"}`)
	signed := SignSessionValue(payload, "old-secret")

	if _, ok := VerifySessionValue(signed, "new-secret", []string{"old-secret"}); !ok {
		t.Fatal("VerifySessionValue() with fallback = false, want true")
	}
	if _, ok := VerifySessionValue(signed, "new-secret", nil); ok {
		t.Fatal("VerifySessionValue() without fallback = true, want false")
	}
}

func TestVerifySessionValueRejectsTampering(t *testing.T) {
	payload := []byte(`{"sub":"user-1"}`)
	signed := SignSessionValue(payload, "secret")

	tampered := signed + "extra"
	if _, ok := VerifySessionValue(tampered, "secret", nil); ok {
		t.Fatal("VerifySessionValue(tampered) = true, want false")
	}

	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	dot := len(signed) - len(testHMACSignature("secret", payloadB64))
	if dot <= 0 {
		t.Fatalf("unexpected signed shape: %q", signed)
	}
	badSignature := signed[:dot] + ".AAAA"
	if _, ok := VerifySessionValue(badSignature, "secret", nil); ok {
		t.Fatal("VerifySessionValue(bad signature) = true, want false")
	}
}

func TestVerifySessionValueRejectsMalformedInput(t *testing.T) {
	for _, signed := range []string{"", "no-dot", "a.b.c"} {
		if _, ok := VerifySessionValue(signed, "secret", nil); ok {
			t.Fatalf("VerifySessionValue(%q) = true, want false", signed)
		}
	}
}

func TestAttachExternalUser(t *testing.T) {
	r := apisixctx.WithApisixVars(httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{})
	userinfo := map[string]any{"name": "alice"}

	AttachExternalUser(r, userinfo, nil)

	if got := apisixctx.GetApisixVars(r)["$external_user"]; got == nil {
		t.Fatal("$external_user var not set")
	}
	if got := r.Header.Get("X-Userinfo"); got == "" {
		t.Fatal("X-Userinfo header not set")
	}

	disabled := false
	r2 := apisixctx.WithApisixVars(httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{})
	AttachExternalUser(r2, userinfo, &disabled)
	if got := r2.Header.Get("X-Userinfo"); got != "" {
		t.Fatalf("X-Userinfo = %q, want empty when set_user_info_header=false", got)
	}
	if got := apisixctx.GetApisixVars(r2)["$external_user"]; got == nil {
		t.Fatal("$external_user var not set when header disabled")
	}
}

func TestCookieSameSite(t *testing.T) {
	tests := []struct {
		value string
		want  http.SameSite
	}{
		{"Strict", http.SameSiteStrictMode},
		{"None", http.SameSiteNoneMode},
		{"Default", http.SameSiteDefaultMode},
		{"Lax", http.SameSiteLaxMode},
		{"", http.SameSiteLaxMode},
		{"Unknown", http.SameSiteLaxMode},
	}
	for _, test := range tests {
		if got := CookieSameSite(test.value); got != test.want {
			t.Fatalf("CookieSameSite(%q) = %v, want %v", test.value, got, test.want)
		}
	}
}

func TestCodeFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?code=query-code", nil)
	r.Header.Set("X-Code", "header-code")

	if got := CodeFromRequest(r, "X-Code", "code"); got != "header-code" {
		t.Fatalf("CodeFromRequest() = %q, want header-code", got)
	}

	r.Header.Del("X-Code")
	if got := CodeFromRequest(r, "X-Code", "code"); got != "query-code" {
		t.Fatalf("CodeFromRequest() = %q, want query-code", got)
	}
}
