package elasticsearch_logger

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBulkResponseBodyKeepsConnectionReusable(t *testing.T) {
	var newConnections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"version":{"number":"8.17.0"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"errors":false,"padding":"`+strings.Repeat("x", 65536)+`"}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"}, Timeout: 2})
	t.Cleanup(p.Stop)
	for range 2 {
		if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/"}}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if got := newConnections.Load(); got != 1 {
		t.Fatalf("two bulk sends opened %d connections; ignoring the response body prevents keep-alive reuse", got)
	}
}
