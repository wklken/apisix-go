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
