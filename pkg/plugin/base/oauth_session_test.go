package base

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

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
