package openid_connect

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOIDCProductionClientConnectFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	discoveryURL := server.URL
	server.Close()

	p := newTestPlugin(t, Config{
		ClientID: "apisix", Discovery: discoveryURL, BearerOnly: true, UseJWKS: true, Timeout: 1,
	})
	if _, err := p.discoveryDoc(); err == nil {
		t.Fatal("discoveryDoc() error = nil for closed discovery endpoint")
	}
}

func TestOIDCProductionClientHonorsConfiguredTimeout(t *testing.T) {
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(requestCanceled)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		ClientID: "apisix", Discovery: server.URL, BearerOnly: true, UseJWKS: true, Timeout: 1,
	})
	started := time.Now()
	if _, err := p.discoveryDoc(); err == nil {
		t.Fatal("discoveryDoc() error = nil for stalled discovery endpoint")
	}
	elapsed := time.Since(started)
	if elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("discovery timeout elapsed = %s, want configured one-second production-client bound", elapsed)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("production client timeout did not cancel discovery request context")
	}
}
