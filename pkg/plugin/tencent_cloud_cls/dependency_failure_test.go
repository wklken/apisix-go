package tencent_cloud_cls

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

func TestSendBatchClassifiesCLSDependencyFailures(t *testing.T) {
	t.Run("connect failure", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host := listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		p := newTestPlugin(t, Config{
			Scheme: "http", CLSHost: host, CLSTopic: "topic-a",
			SecretID: "secret-id", SecretKey: "secret-key", Timeout: 1000,
		})

		_, err = p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "failed to send log to Tencent Cloud CLS endpoint") {
			t.Fatalf("SendBatch() error = %v, want classified CLS connect failure", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			<-release
		}))
		t.Cleanup(server.Close)
		p := newTestPlugin(t, Config{
			Scheme: "http", CLSHost: strings.TrimPrefix(server.URL, "http://"), CLSTopic: "topic-a",
			SecretID: "secret-id", SecretKey: "secret-key", Timeout: 1000,
		})

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := p.SendBatch(ctx, []map[string]any{{"path": "/orders"}}, 1)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendBatch() error = %v, want deadline exceeded", err)
		}
	})
}
