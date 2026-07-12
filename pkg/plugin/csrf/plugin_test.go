package csrf

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
)

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

func TestHandlerRejectsMissingHeaderWithJSONError(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"error_msg":"no csrf token in headers"}` {
		t.Fatalf("body = %q, want APISIX csrf error JSON", got)
	}
}

func TestPostInitRejectsInvalidEncryptedKey(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Key: "plain"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted csrf key rejection")
	}
}

func TestPostInitResolvesEncryptedKey(t *testing.T) {
	key := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Key: encryptCSRFTestValue(t, key, "secret")}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.Key != "secret" {
		t.Fatalf("csrf key = %q, want decrypted value", p.config.Key)
	}
}

func TestPostInitResolvesKeyFromRotatedKeyring(t *testing.T) {
	oldKey := "qeddd145sfvddff3"
	newKey := "1234567890abcdef"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Key: encryptCSRFTestValue(t, oldKey, "rotated-secret")}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.Key != "rotated-secret" {
		t.Fatalf("csrf key = %q, want rotated plaintext", p.config.Key)
	}
}

func TestPostInitRejectsMissingKeyring(t *testing.T) {
	data_encryption.Configure(true, nil)
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Key: "ciphertext"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want missing keyring rejection")
	}
}

func encryptCSRFTestValue(t *testing.T, key string, value string) string {
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

func TestCheckCSRFTokenAllowsExpiredTokenWhenExpiresIsZero(t *testing.T) {
	key := "secret"
	token := csrfToken{
		Random:  0.25,
		Expires: 1,
	}
	token.Sign = genSign(token.Random, token.Expires, key)
	body, err := json.Marshal(token)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}

	if !checkCSRFToken(base64.StdEncoding.EncodeToString(body), key, 0) {
		t.Fatal("checkCSRFToken() = false, want true when expires is zero")
	}
}

func TestPostInitPreservesExplicitZeroExpires(t *testing.T) {
	p := newTestPlugin(t, Config{
		Key:     "secret",
		Expires: int64Ptr(0),
	})

	if got := p.expires(); got != 0 {
		t.Fatalf("expires = %d, want explicit zero preserved", got)
	}
}

//go:fix inline
func int64Ptr(v int64) *int64 {
	return new(v)
}
