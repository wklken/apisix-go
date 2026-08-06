package base

import (
	"net/http"
	"testing"
)

func TestRemoveHTTP2ConnectionHeaders(t *testing.T) {
	header := http.Header{
		"Connection":        {"keep-alive"},
		"Keep-Alive":        {"timeout=5"},
		"Proxy-Connection":  {"keep-alive"},
		"Upgrade":           {"websocket"},
		"Transfer-Encoding": {"chunked"},
		"X-Result":          {"ok"},
	}

	RemoveHTTP2ConnectionHeaders(header)

	for _, field := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade", "Transfer-Encoding"} {
		if got := header.Get(field); got != "" {
			t.Fatalf("%s = %q, want removed", field, got)
		}
	}
	if got := header.Get("X-Result"); got != "ok" {
		t.Fatalf("X-Result = %q, want preserved", got)
	}
}
