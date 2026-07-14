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
