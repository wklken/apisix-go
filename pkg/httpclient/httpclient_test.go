package httpclient

import (
	"net/http"
	"testing"
)

func TestNewTransportDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")

	transport := NewTransport()
	if transport.Proxy != nil {
		t.Fatal("NewTransport().Proxy is non-nil, want environment proxies disabled")
	}
	client := New()
	clientTransport, ok := client.Transport.(*http.Transport)
	if !ok || clientTransport.Proxy != nil {
		t.Fatalf("New().Transport = %#v, want no-proxy *http.Transport", client.Transport)
	}
}

func TestNewTransportDoesNotMutateDefaultTransport(t *testing.T) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not *http.Transport")
	}
	originalDisableKeepAlives := defaultTransport.DisableKeepAlives

	transport := NewTransport()
	transport.DisableKeepAlives = !originalDisableKeepAlives
	if defaultTransport.DisableKeepAlives != originalDisableKeepAlives {
		t.Fatal("mutating NewTransport result changed http.DefaultTransport")
	}
}
