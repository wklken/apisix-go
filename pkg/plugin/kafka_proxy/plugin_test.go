package kafka_proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestPostInitRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{}
	if err := p.PostInit(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("PostInit() error = %v, want missing resolver error", err)
	}
}

func TestHandlerStoresSASLConfigForKafkaUpstream(t *testing.T) {
	p := newTestPlugin(t, Config{
		SASL: &SASL{
			Username: "user",
			Password: "pwd",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !SASLEnabled(r) {
			t.Fatal("SASLEnabled() = false, want true")
		}
		if got := SASLUsername(r); got != "user" {
			t.Fatalf("SASLUsername() = %q, want user", got)
		}
		if got := SASLPassword(r); got != "pwd" {
			t.Fatalf("SASLPassword() = %q, want pwd", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerDoesNotSetSASLContextWhenDisabled(t *testing.T) {
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "/kafka", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SASLEnabled(r) {
			t.Fatal("SASLEnabled() = true, want false")
		}
		if got := SASLUsername(r); got != "" {
			t.Fatalf("SASLUsername() = %q, want empty", got)
		}
		if got := SASLPassword(r); got != "" {
			t.Fatalf("SASLPassword() = %q, want empty", got)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
	}
}

func TestPostInitRejectsInvalidEncryptedSASLPassword(t *testing.T) {
	p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: "plain"}}}
	p.SetDependencies(base.Dependencies{
		DataEncryption: data_encryption.NewService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict SASL password rejection")
	}
}

func TestPostInitResolvesEncryptedSASLPassword(t *testing.T) {
	key := "qeddd145sfvddff3"
	p := &Plugin{config: Config{SASL: &SASL{
		Username: "user",
		Password: encryptKafkaProxyTestValue(t, key, "secret"),
	}}}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(true, []string{key}).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.SASL.Password != "secret" {
		t.Fatalf("SASL password = %q, want decrypted value", p.config.SASL.Password)
	}
}

func TestPostInitResolvesContextualV2SASLPassword(t *testing.T) {
	key := "qeddd145sfvddff3"
	ciphertext, err := data_encryption.EncryptForContext("secret", key, "kafka-proxy.sasl.password")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	p := &Plugin{config: Config{SASL: &SASL{Username: "user", Password: ciphertext}}}
	p.SetDependencies(base.Dependencies{DataEncryption: data_encryption.NewService(true, []string{key}).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.SASL.Password != "secret" {
		t.Fatalf("SASL password = %q, want decrypted value", p.config.SASL.Password)
	}
}

func encryptKafkaProxyTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
