package base

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
)

// SignRawSessionValue signs a payload as base64url(payload) + "." +
// base64url(HMAC-SHA256(payload, secret)). Unlike SignSessionValue, the HMAC
// covers the raw payload bytes, not the encoded payload.
func SignRawSessionValue(payload []byte, secret string) string {
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encodedPayload + "." + signature
}

// VerifyRawSessionValue verifies a raw-payload signed value against secret and
// fallbacks and returns the decoded payload.
func VerifyRawSessionValue(signed, secret string, fallbacks []string) ([]byte, bool) {
	dot := strings.Index(signed, ".")
	if dot <= 0 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(signed[:dot])
	if err != nil {
		return nil, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(signed[dot+1:])
	if err != nil {
		return nil, false
	}

	for _, candidate := range append([]string{secret}, fallbacks...) {
		mac := hmac.New(sha256.New, []byte(candidate))
		mac.Write(payload)
		if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) == 1 {
			return payload, true
		}
	}
	return nil, false
}

// CallbackPath returns the request path of an absolute callback URI, the
// original value for relative or unparsable URIs, and "/" when an absolute URI
// has no path.
func CallbackPath(callbackURI string) string {
	parsed, err := url.Parse(callbackURI)
	if err != nil || !parsed.IsAbs() {
		return callbackURI
	}
	if parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

// Sha256Hex returns the lowercase hex SHA-256 digest of value.
func Sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
