package base

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
)

const (
	oauthSessionVersion       = 1
	maxOAuthSessionCookieSize = 3800
	// OAuthStateLifetime bounds the browser state cookie and replay window.
	OAuthStateLifetime = 5 * time.Minute
)

// NewOAuthState returns a URL-safe, cryptographically random OAuth state
// value. Callers should bind the value to a sealed session before sending it
// to an OAuth provider.
func NewOAuthState() (string, error) {
	state := make([]byte, 32)
	if _, err := rand.Read(state); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(state), nil
}

// OAuthStateReplayCache records successfully consumed state cookies for their
// short lifetime. The zero value is ready for use.
type OAuthStateReplayCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

// Consume returns true once for a state cookie value and false for replays.
func (c *OAuthStateReplayCache) Consume(value string, now time.Time) bool {
	if value == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]time.Time)
	}
	for key, expiresAt := range c.entries {
		if !expiresAt.After(now) {
			delete(c.entries, key)
		}
	}
	if _, exists := c.entries[value]; exists {
		return false
	}
	c.entries[value] = now.Add(OAuthStateLifetime)
	return true
}

type oauthSessionEnvelope struct {
	Version     int    `json:"v"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
	Fingerprint string `json:"fp"`
	Payload     []byte `json:"data"`
}

// SealOAuthSession encrypts a bounded OAuth session using the primary secret.
func SealOAuthSession(
	payload []byte,
	secret string,
	fingerprint string,
	issuedAt time.Time,
	expiresAt time.Time,
) (string, error) {
	if secret == "" || fingerprint == "" || !expiresAt.After(issuedAt) {
		return "", errors.New("invalid OAuth session envelope")
	}
	plaintext, err := json.Marshal(oauthSessionEnvelope{
		Version:     oauthSessionVersion,
		IssuedAt:    issuedAt.Unix(),
		ExpiresAt:   expiresAt.Unix(),
		Fingerprint: fingerprint,
		Payload:     payload,
	})
	if err != nil {
		return "", err
	}
	gcm, err := oauthSessionGCM(secret)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil))
	if len(encoded) > maxOAuthSessionCookieSize {
		return "", errors.New("OAuth session cookie exceeds size limit")
	}
	return encoded, nil
}

// OpenOAuthSession decrypts a bounded OAuth session with the primary secret or
// one of its rotation fallbacks and validates its version, expiry, and config.
func OpenOAuthSession(
	encoded string,
	secret string,
	fallbacks []string,
	fingerprint string,
	now time.Time,
) ([]byte, error) {
	if encoded == "" || len(encoded) > maxOAuthSessionCookieSize || secret == "" || fingerprint == "" {
		return nil, errors.New("invalid OAuth session cookie")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("invalid OAuth session cookie")
	}
	for _, candidate := range append([]string{secret}, fallbacks...) {
		if candidate == "" {
			continue
		}
		gcm, err := oauthSessionGCM(candidate)
		if err != nil || len(sealed) < gcm.NonceSize() {
			continue
		}
		plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
		if err != nil {
			continue
		}
		var envelope oauthSessionEnvelope
		if err := json.Unmarshal(plaintext, &envelope); err != nil {
			return nil, errors.New("invalid OAuth session envelope")
		}
		if envelope.Version != oauthSessionVersion ||
			envelope.Fingerprint != fingerprint ||
			envelope.ExpiresAt <= now.Unix() ||
			envelope.ExpiresAt <= envelope.IssuedAt {
			return nil, errors.New("invalid OAuth session envelope")
		}
		return envelope.Payload, nil
	}
	return nil, errors.New("invalid OAuth session cookie")
}

func oauthSessionGCM(secret string) (cipher.AEAD, error) {
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SignSessionValue signs a payload for a session cookie as
// base64url(payload) + "." + base64url(HMAC-SHA256(base64url(payload), secret)).
func SignSessionValue(value []byte, secret string) string {
	payload := base64.RawURLEncoding.EncodeToString(value)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + signature
}

// VerifySessionValue verifies a signed value against secret and fallbacks and
// returns the decoded payload.
func VerifySessionValue(signed, secret string, fallbacks []string) ([]byte, bool) {
	dot := strings.LastIndexByte(signed, '.')
	if dot < 0 {
		return nil, false
	}
	payload := signed[:dot]
	signature, err := base64.RawURLEncoding.DecodeString(signed[dot+1:])
	if err != nil {
		return nil, false
	}
	for _, candidate := range append([]string{secret}, fallbacks...) {
		mac := hmac.New(sha256.New, []byte(candidate))
		mac.Write([]byte(payload))
		expected := mac.Sum(nil)
		if subtle.ConstantTimeCompare(signature, expected) == 1 {
			decoded, err := base64.RawURLEncoding.DecodeString(payload)
			return decoded, err == nil
		}
	}
	return nil, false
}

// AttachExternalUser publishes userinfo into the $external_user request
// variable and the X-Userinfo header unless setHeader is non-nil and false.
func AttachExternalUser(r *http.Request, userinfo map[string]any, setHeader *bool) {
	if vars := apisixctx.GetApisixVars(r); vars != nil {
		vars["$external_user"] = userinfo
	}
	if setHeader != nil && !*setHeader {
		return
	}
	raw, err := json.Marshal(userinfo)
	if err != nil {
		return
	}
	r.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

// CodeFromRequest reads an authorization code from the named header first,
// falling back to the named query parameter.
func CodeFromRequest(r *http.Request, headerName, queryName string) string {
	apisixctx.RegisterSensitiveQueryName(r, queryName)
	if code := r.Header.Get(headerName); code != "" {
		return code
	}
	return r.URL.Query().Get(queryName)
}

// CookieSameSite maps an OAuth session cookie_same_site setting to its
// net/http constant, defaulting to Lax for empty or unknown values.
func CookieSameSite(value string) http.SameSite {
	switch value {
	case "Strict":
		return http.SameSiteStrictMode
	case "None":
		return http.SameSiteNoneMode
	case "Default":
		return http.SameSiteDefaultMode
	default:
		return http.SameSiteLaxMode
	}
}
