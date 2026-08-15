package csrf

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
)

func TestCSRFPluginSourceGuardRejectsDirectComparison(t *testing.T) {
	source, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("read plugin.go: %v", err)
	}
	if bytes.Contains(source, []byte("sign != csrfToken.Sign")) {
		t.Fatal("plugin.go compares token signatures with a direct non-constant-time expression")
	}
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
	if got := rr.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q, want application/json with UTF-8 charset", got)
	}
	if got := rr.Body.String(); got != `{"error_msg":"no csrf token in headers"}` {
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

func TestGenCSRFTokenUsesInjectedReader(t *testing.T) {
	reader := bytes.NewReader(bytes.Repeat([]byte{0xff}, 8))
	tokenValue, err := genCSRFToken("secret", reader)
	if err != nil {
		t.Fatalf("genCSRFToken() error = %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(tokenValue)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	var token csrfToken
	if err := json.Unmarshal(decoded, &token); err != nil {
		t.Fatalf("unmarshal token: %v", err)
	}

	wantRandom := float64((uint64(1)<<53)-1) / float64(uint64(1)<<53)
	if token.Random != wantRandom {
		t.Fatalf("random = %.17g, want %.17g", token.Random, wantRandom)
	}
	if token.Random < 0 || token.Random >= 1 {
		t.Fatalf("random = %.17g, want value in [0, 1)", token.Random)
	}
	if token.Sign != genSign(token.Random, token.Expires, "secret") {
		t.Fatal("generated token signature does not match its serialized fields")
	}
}

func TestHandlerFailsClosedWhenEntropyReadFails(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
	p.randomReader = bytes.NewReader(nil)

	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler should not run when csrf entropy fails")
	})).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://example.com/safe", nil))

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if got := strings.TrimSpace(response.Body.String()); got != `{"error_msg":"failed to generate csrf token"}` {
		t.Fatalf("body = %q, want generic csrf generation error", got)
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("response set a csrf cookie after entropy failure")
	}
}

func TestPostInitPreservesExplicitZeroExpires(t *testing.T) {
	p := newTestPlugin(t, Config{
		Key:     "secret",
		Expires: new(int64(0)),
	})

	if got := p.expires(); got != 0 {
		t.Fatalf("expires = %d, want explicit zero preserved", got)
	}
}

func TestCheckCSRFTokenValidationTable(t *testing.T) {
	key := "secret"
	now := time.Now().Unix()
	valid := csrfToken{Random: 0.25, Expires: now, Sign: genSign(0.25, now, key)}
	validBody, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal valid token: %v", err)
	}

	expired := csrfToken{Random: 0.5, Expires: now - 7300, Sign: genSign(0.5, now-7300, key)}
	expiredBody, err := json.Marshal(expired)
	if err != nil {
		t.Fatalf("marshal expired token: %v", err)
	}

	wrongKey := csrfToken{Random: 0.75, Expires: now, Sign: genSign(0.75, now, "other-key")}
	wrongKeyBody, err := json.Marshal(wrongKey)
	if err != nil {
		t.Fatalf("marshal wrong-signature token: %v", err)
	}

	shortSign := csrfToken{Random: 0.8, Expires: now, Sign: "short-signature"}
	shortSignBody, err := json.Marshal(shortSign)
	if err != nil {
		t.Fatalf("marshal short-signature token: %v", err)
	}

	longSign := csrfToken{Random: 0.9, Expires: now, Sign: strings.Repeat("a", 128)}
	longSignBody, err := json.Marshal(longSign)
	if err != nil {
		t.Fatalf("marshal long-signature token: %v", err)
	}

	tests := []struct {
		name      string
		token     string
		key       string
		expires   int64
		wantValid bool
	}{
		{
			name:      "valid signature",
			token:     base64.StdEncoding.EncodeToString(validBody),
			key:       key,
			expires:   7200,
			wantValid: true,
		},
		{name: "invalid base64", token: "!!!not-base64!!!", key: key, expires: 7200},
		{name: "invalid json", token: base64.StdEncoding.EncodeToString([]byte("{not json")), key: key, expires: 7200},
		{name: "expired timestamp", token: base64.StdEncoding.EncodeToString(expiredBody), key: key, expires: 7200},
		{name: "wrong signature", token: base64.StdEncoding.EncodeToString(wrongKeyBody), key: key, expires: 7200},
		{
			name:    "wrong signature shorter",
			token:   base64.StdEncoding.EncodeToString(shortSignBody),
			key:     key,
			expires: 7200,
		},
		{
			name:    "wrong signature longer",
			token:   base64.StdEncoding.EncodeToString(longSignBody),
			key:     key,
			expires: 7200,
		},
		{
			name:      "expires zero bypass",
			token:     base64.StdEncoding.EncodeToString(expiredBody),
			key:       key,
			expires:   0,
			wantValid: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := checkCSRFToken(test.token, test.key, test.expires); got != test.wantValid {
				t.Fatalf("checkCSRFToken() = %t, want %t", got, test.wantValid)
			}
		})
	}
}

func TestHandlerValidPostRefreshesCookie(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
	token, err := genCSRFToken("secret", bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Fatalf("genCSRFToken() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
	request.Header.Set("csrf-token", token)
	request.AddCookie(&http.Cookie{Name: "csrf-token", Value: token})

	called := false
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called/status = %t/%d, want true/204", called, response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("Set-Cookie count = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "csrf-token" || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v, want name csrf-token, path /, SameSite Lax", cookie)
	}
}

func TestHandlerRejectsInvalidRequestsWithJSONErrors(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		cookie   string
		wantBody string
	}{
		{name: "missing cookie", header: "token", wantBody: `{"error_msg":"no csrf cookie"}`},
		{
			name:     "mismatch",
			header:   "header-token",
			cookie:   "cookie-token",
			wantBody: `{"error_msg":"csrf token mismatch"}`,
		},
		{
			name:     "invalid signature",
			header:   "forged",
			cookie:   "forged",
			wantBody: `{"error_msg":"Failed to verify the csrf token signature"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
			request := httptest.NewRequest(http.MethodPost, "http://example.com/post", nil)
			request.Header.Set("csrf-token", test.header)
			if test.cookie != "" {
				request.AddCookie(&http.Cookie{Name: "csrf-token", Value: test.cookie})
			}

			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("next handler should not run for an invalid token")
			})).ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
			if got := strings.TrimSpace(response.Body.String()); got != test.wantBody {
				t.Fatalf("body = %q, want %q", got, test.wantBody)
			}
		})
	}
}

func TestHandlerSafeMethodsSetNewCookieAndContinue(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			p := newTestPlugin(t, Config{Key: "secret", Name: "csrf-token"})
			called := false
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(response, httptest.NewRequest(method, "http://example.com/safe", nil))

			if !called || response.Code != http.StatusNoContent {
				t.Fatalf("called/status = %t/%d, want true/204", called, response.Code)
			}
			cookies := response.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != "csrf-token" {
				t.Fatalf("cookies = %#v, want one refreshed csrf-token cookie", cookies)
			}
		})
	}
}

func TestPluginConfigDefaultsAndIdentity(t *testing.T) {
	p := newTestPlugin(t, Config{Key: "secret"})
	if got := p.Config(); got != &p.config {
		t.Fatal("Config() does not return the plugin config pointer")
	}
	if p.config.Name != "apisix-csrf-token" {
		t.Fatalf("default name = %q, want apisix-csrf-token", p.config.Name)
	}
	if p.expires() != defaultCSRFExpires {
		t.Fatalf("default expires = %d, want %d", p.expires(), defaultCSRFExpires)
	}
	if p.randomReader == nil {
		t.Fatal("random reader is nil after PostInit()")
	}
}
