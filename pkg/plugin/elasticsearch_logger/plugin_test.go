package elasticsearch_logger

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
	})

	if p.config.Timeout != 10 {
		t.Fatalf("timeout = %d, want official default 10 seconds", p.config.Timeout)
	}
	if p.config.SslVerify == nil || !*p.config.SslVerify {
		t.Fatalf("ssl_verify = %v, want true", p.config.SslVerify)
	}
}

func TestSendWritesBulkNDJSONWithHeadersAndAuth(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("X-Cluster") != "logs" {
			t.Fatalf("X-Cluster = %q, want logs", r.Header.Get("X-Cluster"))
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("elastic:secret"))
		if r.Header.Get("Authorization") != wantAuth {
			t.Fatalf("Authorization = %q, want %q", r.Header.Get("Authorization"), wantAuth)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Auth:          &AuthConfig{Username: "elastic", Password: "secret"},
		Headers:       map[string]string{"X-Cluster": "logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `{"index":{"_index":"apisix-logs"}}`+"\n") {
			t.Fatalf("bulk body = %q, want index action", body)
		}
		if !strings.Contains(body, `"path":"/orders"`) {
			t.Fatalf("bulk body = %q, want log document", body)
		}
		if !strings.HasSuffix(body, "\n") {
			t.Fatalf("bulk body = %q, want trailing newline", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSchemaAcceptsOfficialEndpointAddrHeadersAndBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":       "http://127.0.0.1:9200",
		"field":               map[string]any{"index": "apisix"},
		"headers":             map[string]any{"X-Cluster": "logs"},
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official config fields: %v", err)
	}
}
