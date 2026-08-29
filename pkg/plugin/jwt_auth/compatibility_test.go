package jwt_auth

import (
	"strings"
	"testing"
	"time"
)

func TestAPISIX317RejectsInsecure512BitRSACompatibilityKey(t *testing.T) {
	const publicKey = `-----BEGIN PUBLIC KEY-----
MFwwDQYJKoZIhvcNAQEBBQADSwAwSAJBAKebDxlvQMGyEesAL1r1nIJBkSdqu3Hr
7noq/0ukiZqVQLSJPMOv0oxQSutvvK3hoibwGakDOza+xRITB7cs2cECAwEAAQ==
-----END PUBLIC KEY-----`
	const token = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJrZXkiOiJ1c2VyLWtleS1yczI1NiIsIm5iZiI6MTcyNzI3NDk4M30." +
		"FaV6N-bWaSXkRrF2ec28hH5QENl-8I0LCONdNnQpB1YOb4akP-lKnwtABgfsQ_eKaEIf1PWNoghyByLejXaPbQ"

	_, err := verifyToken(token, consumerConfig{
		Key:       "user-key-rs256",
		Algorithm: "RS256",
		PublicKey: publicKey,
	}, time.Unix(1_800_000_000, 0), 0, nil)
	if err == nil {
		t.Fatal("verifyToken() accepted the upstream 512-bit RSA compatibility token")
	}
	if !strings.Contains(err.Error(), "512-bit keys are insecure") {
		t.Fatalf("verifyToken() error = %q, want Go RSA key-size floor", err)
	}
}
