package splunk_hec_logging

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendBatchClassifiesSplunkDependencyFailures(t *testing.T) {
	t.Run("malformed rejection", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not-json"))
		}))
		t.Cleanup(server.Close)
		p := newTestPlugin(t, Config{Endpoint: Endpoint{URI: server.URL, Token: "secret", Timeout: 1}})

		_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "status code [503]") ||
			!strings.Contains(err.Error(), "not-json") {
			t.Fatalf("SendBatch() error = %v, want Splunk status and malformed rejection body", err)
		}
	})

	t.Run("connect failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := "http://" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		p := newTestPlugin(t, Config{Endpoint: Endpoint{URI: endpoint, Token: "secret", Timeout: 1}})

		_, err = p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "failed to send log to Splunk HEC endpoint") {
			t.Fatalf("SendBatch() error = %v, want classified Splunk connect failure", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			<-release
		}))
		t.Cleanup(server.Close)
		p := newTestPlugin(t, Config{Endpoint: Endpoint{URI: server.URL, Token: "secret", Timeout: 1}})

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := p.SendBatch(ctx, []map[string]any{{"path": "/orders"}}, 1)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendBatch() error = %v, want deadline exceeded", err)
		}
	})
}
