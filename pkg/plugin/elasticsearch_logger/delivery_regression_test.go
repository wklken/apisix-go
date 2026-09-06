package elasticsearch_logger

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeliveryRetriesVersionProbeAndUsesOfficialAccept(t *testing.T) {
	var infos atomic.Int32
	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			infos.Add(1)
			if !healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"version":{"number":"6.8.0"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"_type":"_doc"`) {
			t.Errorf("ES6 bulk action lacks type: %s", body)
		}
		if r.Header.Get("Accept") != "application/vnd.elasticsearch+json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"errors":true,"items":[{"index":{"status":429}}]}`))
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"}, Timeout: 2})
	if p.elasticsearchVersion() != "" {
		t.Fatal("failed initial probe populated version")
	}
	initialProbes := infos.Load()
	healthy.Store(true)
	for range 2 {
		if first, err := p.SendBatch(
			context.Background(),
			[]map[string]any{{"path": "/"}},
			1,
		); err != nil ||
			first != 0 {
			t.Fatalf("HTTP200 bulk result = %d, %v", first, err)
		}
	}
	if infos.Load() != initialProbes+1 || p.elasticsearchVersion() != "6" {
		t.Fatalf("version probes = %d, version = %q", infos.Load(), p.elasticsearchVersion())
	}
}
