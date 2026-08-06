package base

import "net/http"

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
