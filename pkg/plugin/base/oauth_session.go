package base

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
)

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
	if code := r.Header.Get(headerName); code != "" {
		return code
	}
	return r.URL.Query().Get(queryName)
}
