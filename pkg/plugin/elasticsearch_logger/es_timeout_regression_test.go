package elasticsearch_logger

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestVersionProbeKeepsBulkRequestTimeout(t *testing.T) {
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			if !healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			time.Sleep(650 * time.Millisecond)
			_, _ = io.WriteString(w, `{"version":{"number":"8.17.0"}}`)
			return
		}
		time.Sleep(550 * time.Millisecond)
		_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"}, Timeout: 1})
	t.Cleanup(p.Stop)
	healthy.Store(true)
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/"}}, 1); err != nil {
		t.Fatalf("version and bulk each completed within the configured one-second request timeout: %v", err)
	}
}
