package elasticsearch_logger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConfiguredAcceptOverridesBulkDefault(t *testing.T) {
	const configured = "application/vnd.elasticsearch+json;compatible-with=7"
	seen := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"version":{"number":"8.0.0"}}`))
			return
		}
		seen <- r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	defer server.Close()
	p := newTestPlugin(
		t,
		Config{
			EndpointAddrs: []string{server.URL},
			Field:         FieldConfig{Index: "logs"},
			Timeout:       2,
			Headers:       map[string]string{"Accept": configured},
		},
	)
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"x": 1}}, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != configured {
		t.Fatalf("Accept=%q, want configured override %q", got, configured)
	}
}
