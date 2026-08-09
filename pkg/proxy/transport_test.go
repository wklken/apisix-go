package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewTransportDoesNotAutoDecompressUpstreamResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "plain upstream bytes")
	}))
	defer upstream.Close()

	client := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).Build())}
	response, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read upstream body: %v", err)
	}
	if string(body) != "plain upstream bytes" {
		t.Fatalf("body = %q, want raw upstream bytes", body)
	}
	if got := response.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip preserved", got)
	}
}

func TestNewTransportAppliesConnectionCaps(t *testing.T) {
	transport := NewTransport((&TransportOptionBuilder{}).
		WithMaxIdleConnections(64).
		WithMaxIdleConnectionsPerHost(16).
		WithMaxConnectionsPerHost(32).
		Build())
	if transport.MaxIdleConns != 64 || transport.MaxIdleConnsPerHost != 16 || transport.MaxConnsPerHost != 32 {
		t.Fatalf(
			"transport caps = %d/%d/%d",
			transport.MaxIdleConns,
			transport.MaxIdleConnsPerHost,
			transport.MaxConnsPerHost,
		)
	}
}

func TestNewTransportHonorsTLSVerification(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	insecure := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).WithInsecureSkipVerify(true).Build())}
	response, err := insecure.Get(upstream.URL)
	if err != nil {
		t.Fatalf("insecure transport GET: %v", err)
	}
	_ = response.Body.Close()

	verified := &http.Client{Transport: NewTransport((&TransportOptionBuilder{}).WithInsecureSkipVerify(false).Build())}
	if _, err := verified.Get(upstream.URL); err == nil {
		t.Fatal("verified transport accepted an untrusted certificate")
	}
}
