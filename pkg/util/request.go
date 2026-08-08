package util

import "net/http"

// RequestSize returns the declared Content-Length when known, and zero for
// chunked or absent request bodies.
func RequestSize(r *http.Request) int64 {
	if r.ContentLength > 0 {
		return r.ContentLength
	}
	return 0
}
