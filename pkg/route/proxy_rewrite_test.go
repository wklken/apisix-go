package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApplyProxyRewriteURIUpdatesPathAndQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/old?keep=0", nil)

	applyProxyRewriteURI(req, "/private/v1?token=redacted")

	if req.URL.Path != "/private/v1" {
		t.Fatalf("path = %q, want /private/v1", req.URL.Path)
	}
	if req.URL.RawQuery != "token=redacted" {
		t.Fatalf("raw query = %q, want token=redacted", req.URL.RawQuery)
	}
}

func TestApplyProxyRewriteURIPreservesEscapedPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/old", nil)

	applyProxyRewriteURI(req, "/private/%2Fraw?token=redacted")

	if req.URL.Path != "/private//raw" {
		t.Fatalf("path = %q, want decoded path /private//raw", req.URL.Path)
	}
	if req.URL.RawPath != "/private/%2Fraw" {
		t.Fatalf("raw path = %q, want /private/%%2Fraw", req.URL.RawPath)
	}
	if got := req.URL.EscapedPath(); got != "/private/%2Fraw" {
		t.Fatalf("escaped path = %q, want /private/%%2Fraw", got)
	}
	if got := req.URL.RequestURI(); got != "/private/%2Fraw?token=redacted" {
		t.Fatalf("request URI = %q, want encoded path and query", got)
	}
}
