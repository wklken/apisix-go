package openid_connect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestSessionStorageTTL(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		session sessionData
		cfg     SessionConfig
		want    time.Duration
	}{
		{name: "no timeouts", cfg: SessionConfig{}},
		{
			name:    "absolute only",
			session: sessionData{CreatedAt: now.Add(-time.Minute).Unix()},
			cfg:     SessionConfig{AbsoluteTimeout: 120},
			want:    59 * time.Second,
		},
		{
			name: "rolling only",
			cfg:  SessionConfig{RollingTimeout: 60},
			want: 60 * time.Second,
		},
		{
			name: "idling only",
			cfg:  SessionConfig{IdlingTimeout: 30},
			want: 30 * time.Second,
		},
		{
			name:    "earliest timeout wins",
			session: sessionData{CreatedAt: now.Add(-time.Minute).Unix()},
			cfg: SessionConfig{
				AbsoluteTimeout: 120,
				RollingTimeout:  60,
				IdlingTimeout:   30,
			},
			want: 30 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plugin := &Plugin{config: Config{Session: test.cfg}}
			got := plugin.sessionStorageTTL(test.session)
			if test.want == 0 {
				if got != 0 {
					t.Fatalf("sessionStorageTTL() = %s, want no expiry", got)
				}
				return
			}
			if got <= 0 || got < test.want-2*time.Second || got > test.want+2*time.Second {
				t.Fatalf("sessionStorageTTL() = %s, want within 2s of %s", got, test.want)
			}
		})
	}
}

func TestValidClientAuthMethod(t *testing.T) {
	for _, method := range []string{"client_secret_basic", "client_secret_post", "private_key_jwt", "client_secret_jwt"} {
		if !validClientAuthMethod(method) {
			t.Fatalf("validClientAuthMethod(%q) = false, want true", method)
		}
	}
	if validClientAuthMethod("unsupported") {
		t.Fatal("validClientAuthMethod(unsupported) = true, want false")
	}
}

func TestParseRSAPrivateKey(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	pkcs1DER := x509.MarshalPKCS1PrivateKey(privateKey)
	pkcs1PEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: pkcs1DER})
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})

	for name, bytes := range map[string][]byte{
		"pkcs1 pem": pkcs1PEM,
		"pkcs1 der": pkcs1DER,
		"pkcs8 pem": pkcs8PEM,
		"pkcs8 der": pkcs8DER,
	} {
		t.Run(name, func(t *testing.T) {
			parsed, err := parseRSAPrivateKey(bytes)
			if err != nil {
				t.Fatalf("parseRSAPrivateKey() error = %v", err)
			}
			if parsed.N.Cmp(privateKey.N) != 0 {
				t.Fatal("parsed key modulus does not match the generated key")
			}
		})
	}

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(ecdsa) error = %v", err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey(ecdsa) error = %v", err)
	}
	if _, err := parseRSAPrivateKey(ecDER); err == nil {
		t.Fatal("parseRSAPrivateKey(ecdsa) error = nil, want non-RSA rejection")
	}
	if _, err := parseRSAPrivateKey([]byte("not a key")); err == nil {
		t.Fatal("parseRSAPrivateKey(garbage) error = nil")
	}
}
