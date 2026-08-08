package base

import (
	"testing"
)

func TestSignAndVerifyRawSessionValue(t *testing.T) {
	payload := []byte("/foo?bar=baz")
	signed := SignRawSessionValue(payload, "secret")

	if want := "L2Zvbz9iYXI9YmF6.bDaB7-YNGmnclqSNCWCvoCjvdK1nlButy0shAb2nKV0"; signed != want {
		t.Fatalf("SignRawSessionValue() = %q, want golden %q", signed, want)
	}

	decoded, ok := VerifyRawSessionValue(signed, "secret", nil)
	if !ok {
		t.Fatal("VerifyRawSessionValue() = false, want true")
	}
	if string(decoded) != string(payload) {
		t.Fatalf("decoded payload = %q, want %q", decoded, payload)
	}
}

func TestVerifyRawSessionValueFallbackSecret(t *testing.T) {
	signed := SignRawSessionValue([]byte("user"), "old-secret")

	if want := "dXNlcg.UanFbzitmDHiSDK3PoIfmHbinJ0GcI7BEr450aIHOwk"; signed != want {
		t.Fatalf("SignRawSessionValue() = %q, want golden %q", signed, want)
	}

	if _, ok := VerifyRawSessionValue(signed, "new-secret", []string{"old-secret"}); !ok {
		t.Fatal("VerifyRawSessionValue() with fallback = false, want true")
	}
	if _, ok := VerifyRawSessionValue(signed, "new-secret", nil); ok {
		t.Fatal("VerifyRawSessionValue() without fallback = true, want false")
	}
}

func TestVerifyRawSessionValueRejectsTampering(t *testing.T) {
	payload := []byte("user")
	signed := SignRawSessionValue(payload, "secret")

	tamperedPayload := signed[:1] + "x" + signed[2:]
	if _, ok := VerifyRawSessionValue(tamperedPayload, "secret", nil); ok {
		t.Fatal("VerifyRawSessionValue(tampered payload) = true, want false")
	}

	tamperedSignature := signed[:len(signed)-1] + "A"
	if _, ok := VerifyRawSessionValue(tamperedSignature, "secret", nil); ok {
		t.Fatal("VerifyRawSessionValue(tampered signature) = true, want false")
	}
}

func TestVerifyRawSessionValueRejectsMalformedInput(t *testing.T) {
	for _, signed := range []string{"", "no-dot", ".sig", "payload.", "a.b.c"} {
		if _, ok := VerifyRawSessionValue(signed, "secret", nil); ok {
			t.Fatalf("VerifyRawSessionValue(%q) = true, want false", signed)
		}
	}
}

func TestRawSessionValueUsesDifferentWireFormatThanSessionValue(t *testing.T) {
	payload := []byte("user")

	raw := SignRawSessionValue(payload, "secret")
	encoded := SignSessionValue(payload, "secret")

	if raw == encoded {
		t.Fatalf("raw and encoded formats must differ, both = %q", raw)
	}
	if _, ok := VerifyRawSessionValue(encoded, "secret", nil); ok {
		t.Fatal("VerifyRawSessionValue() accepted encoded-payload format, want false")
	}
}

func TestCallbackPath(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want string
	}{
		{name: "absolute with path", uri: "http://example.com/callback?code=x", want: "/callback"},
		{name: "absolute without path", uri: "http://example.com", want: "/"},
		{name: "absolute root path", uri: "https://example.com/", want: "/"},
		{name: "relative URI passes through", uri: "/callback", want: "/callback"},
		{name: "unparsable URI passes through", uri: "://bad", want: "://bad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CallbackPath(tt.uri); got != tt.want {
				t.Fatalf("CallbackPath(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}

func TestSha256Hex(t *testing.T) {
	if got := Sha256Hex("hello"); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("Sha256Hex(hello) = %q, want golden digest", got)
	}
}
