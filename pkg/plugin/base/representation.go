package base

import (
	"net/http"
	"strings"
)

var bodyDerivedHeaderNames = [...]string{
	"Content-Length",
	"Content-Encoding",
	"Content-Range",
	"Content-MD5",
	"Digest",
	"Content-Digest",
	"Repr-Digest",
	"ETag",
	"Last-Modified",
}

// InvalidateBodyDerivedHeaders removes representation metadata that no longer
// describes the body after a semantic replacement. Iterate the actual map
// keys because Header.Del only removes the canonical key.
func InvalidateBodyDerivedHeaders(header http.Header) {
	for actual := range header {
		for _, field := range bodyDerivedHeaderNames {
			if strings.EqualFold(actual, field) {
				delete(header, actual)
				break
			}
		}
	}
}

// AppendVaryToken adds token to all existing Vary field-values once, using a
// case-insensitive comparison while retaining unrelated tokens and order.
func AppendVaryToken(header http.Header, token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}

	var tokens []string
	seen := make(map[string]struct{})
	for actual, values := range header {
		if !strings.EqualFold(actual, "Vary") {
			continue
		}
		for _, value := range values {
			for item := range strings.SplitSeq(value, ",") {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				key := strings.ToLower(item)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				tokens = append(tokens, item)
			}
		}
		delete(header, actual)
	}

	if _, ok := seen[strings.ToLower(token)]; !ok {
		tokens = append(tokens, token)
	}
	header.Set("Vary", strings.Join(tokens, ", "))
}

// ResponseAllowsBody reports whether method/status permits response body
// bytes. 101 is a final switching-protocols response and is bodyless here.
func ResponseAllowsBody(method string, status int) bool {
	if strings.EqualFold(method, http.MethodHead) {
		return false
	}
	if status >= 100 && status <= 199 {
		return false
	}
	return status != http.StatusNoContent && status != http.StatusNotModified
}
