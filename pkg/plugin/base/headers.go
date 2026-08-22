package base

import (
	"net/http"
	"strings"
)

// IsHopByHopHeader reports whether field is a connection-scoped hop-by-hop
// header from the RFC 7230 set, plus Proxy-Connection.
func IsHopByHopHeader(field string) bool {
	switch strings.ToLower(field) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// RemoveHTTP2ConnectionHeaders removes response headers that cannot be
// forwarded on an HTTP/2 downstream connection.
func RemoveHTTP2ConnectionHeaders(header http.Header) {
	for _, field := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Connection",
		"Upgrade",
		"Transfer-Encoding",
	} {
		header.Del(field)
	}
}
