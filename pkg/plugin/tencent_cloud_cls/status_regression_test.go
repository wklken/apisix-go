package tencent_cloud_cls

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNonRetryableCLSStatusesCompleteDelivery(t *testing.T) {
	for _, status := range []int{401, 403, 404, 413} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)
			p := newTestPlugin(t, Config{
				Scheme:    "http",
				CLSHost:   strings.TrimPrefix(server.URL, "http://"),
				CLSTopic:  "topic",
				SecretID:  "id",
				SecretKey: "key",
			})
			_, err := p.SendBatch(context.Background(), []map[string]any{{"k": "v"}}, 1)
			if err != nil {
				t.Fatalf("status %d: SendBatch error=%v, want completed non-retryable delivery", status, err)
			}
		})
	}
}
