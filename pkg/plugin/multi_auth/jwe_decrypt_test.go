package multi_auth

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestHandlerAcceptsValidJWEDecryptAuthentication(t *testing.T) {
	const (
		consumerName = "multi-auth-jwe-user"
		keyID        = "multi-auth-jwe-key"
		secret       = "12345678901234567890123456789012"
		plaintext    = "Bearer upstream-token"
	)
	addAuthConsumer(t, consumerName, map[string]any{
		"jwe-decrypt": map[string]any{
			"key":    keyID,
			"secret": secret,
		},
	})
	waitForConsumerKey(t, "jwe-decrypt", keyID)

	p := newTestPlugin(t, Config{AuthPlugins: []AuthPluginConfig{
		{"jwe-decrypt": {"header": "Authorization", "forward_header": "Authorization"}},
		{"key-auth": {"header": "apikey"}},
	}})
	req := newMultiAuthRequest()
	req.Header.Set("Authorization", "Bearer "+makeMultiAuthCompactJWE(t, keyID, []byte(secret), plaintext))
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, ok := ctx.AuthenticationStateFrom(r)
		if !ok || state.Source != "jwe-decrypt" || state.Consumer().Username != consumerName {
			t.Fatalf("authentication state = %#v, %v; want jwe-decrypt consumer %q", state, ok, consumerName)
		}
		if got := r.Header.Get("Authorization"); got != plaintext {
			t.Fatalf("Authorization = %q, want decrypted %q", got, plaintext)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", res.Code, res.Body.String())
	}
}

func makeMultiAuthCompactJWE(t *testing.T, keyID string, secret []byte, plaintext string) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{
		"alg": "dir",
		"enc": "A256GCM",
		"kid": keyID,
	})
	if err != nil {
		t.Fatalf("marshal JWE header: %v", err)
	}
	iv := []byte("123456789012")
	block, err := aes.NewCipher(secret)
	if err != nil {
		t.Fatalf("create JWE cipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create JWE GCM: %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagStart := len(sealed) - gcm.Overhead()
	return strings.Join([]string{
		base64.RawURLEncoding.EncodeToString(header),
		"",
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(sealed[:tagStart]),
		base64.RawURLEncoding.EncodeToString(sealed[tagStart:]),
	}, ".")
}
