package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
)

func TestApplyTrafficSplitOverrideUpdatesProxyTarget(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://route.example.com/get", nil)
	req = traffic_split.WithOverride(req, &traffic_split.Override{
		Scheme: "https",
		Host:   "shadow.example.com:9443",
	})

	applyTrafficSplitOverride(req)

	if req.URL.Scheme != "https" {
		t.Fatalf("scheme = %q, want https", req.URL.Scheme)
	}
	if req.URL.Host != "shadow.example.com:9443" {
		t.Fatalf("URL host = %q, want shadow.example.com:9443", req.URL.Host)
	}
	if req.Host != "shadow.example.com:9443" {
		t.Fatalf("Host = %q, want shadow.example.com:9443", req.Host)
	}
}
