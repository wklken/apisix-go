package base

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestParseJWT(t *testing.T) {
	header := map[string]any{"alg": "HS256", "typ": "JWT"}
	payload := map[string]any{"sub": "test-user", "exp": float64(1750000000)}
	headerBytes, _ := json.Marshal(header)
	payloadBytes, _ := json.Marshal(payload)
	raw := base64.RawURLEncoding.EncodeToString(headerBytes) + "." +
		base64.RawURLEncoding.EncodeToString(payloadBytes) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("signature-bytes"))

	token, err := ParseJWT(raw)
	if err != nil {
		t.Fatalf("ParseJWT() error = %v", err)
	}
	if got := token.Signing; got != base64.RawURLEncoding.EncodeToString(
		headerBytes,
	)+"."+base64.RawURLEncoding.EncodeToString(
		payloadBytes,
	) {
		t.Fatalf("Signing = %q, want header.payload", got)
	}
	if got := string(token.Signature); got != "signature-bytes" {
		t.Fatalf("Signature = %q, want signature-bytes", got)
	}
	if got := token.Header["alg"]; got != "HS256" {
		t.Fatalf("Header alg = %v, want HS256", got)
	}
	if got := token.Payload["sub"]; got != "test-user" {
		t.Fatalf("Payload sub = %v, want test-user", got)
	}
}

func TestParseJWTRejectsInvalidInput(t *testing.T) {
	validPart := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	cases := map[string]string{
		"two parts":   validPart + "." + validPart,
		"empty":       "",
		"bad base64":  "!!.!!.!!",
		"bad header":  "not-json." + validPart + "." + validPart,
		"bad payload": validPart + ".not-json." + validPart,
	}
	for name, raw := range cases {
		if _, err := ParseJWT(raw); err == nil {
			t.Fatalf("ParseJWT(%s) = nil error, want error", name)
		}
	}
}

func TestNumberClaim(t *testing.T) {
	if got, ok := NumberClaim(float64(42)); !ok || got != 42 {
		t.Fatalf("NumberClaim(float64(42)) = %d, %v; want 42, true", got, ok)
	}
	if got, ok := NumberClaim(int64(7)); !ok || got != 7 {
		t.Fatalf("NumberClaim(int64(7)) = %d, %v; want 7, true", got, ok)
	}
	if got, ok := NumberClaim(9); !ok || got != 9 {
		t.Fatalf("NumberClaim(int(9)) = %d, %v; want 9, true", got, ok)
	}
	for _, value := range []any{"42", true, nil, []any{1}} {
		if got, ok := NumberClaim(value); ok {
			t.Fatalf("NumberClaim(%#v) = %d, true; want false", value, got)
		}
	}
}
