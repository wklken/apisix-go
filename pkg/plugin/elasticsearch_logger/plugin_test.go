package elasticsearch_logger

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestSendWritesBulkNDJSONWithHeadersAndAuth(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
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

func TestSendBatchWritesMultipleBulkEntries(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
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
		BatchMaxSize:  2,
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-received:
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if len(lines) != 4 {
			t.Fatalf("bulk lines = %d, want 4, body = %q", len(lines), body)
		}
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("bulk body = %q, want both documents", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch batch bulk request")
	}
}

func TestSendSelectsRandomEndpointAddr(t *testing.T) {
	firstRequests := make(chan struct{}, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(first.Close)

	secondRequests := make(chan struct{}, 1)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		secondRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(second.Close)

	oldRandomEndpointIndex := randomEndpointIndex
	randomEndpointIndex = func(n int) int {
		if n != 2 {
			t.Fatalf("random endpoint count = %d, want 2", n)
		}
		return 1
	}
	t.Cleanup(func() {
		randomEndpointIndex = oldRandomEndpointIndex
	})

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{first.URL, second.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case <-secondRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for selected Elasticsearch endpoint")
	}

	select {
	case <-firstRequests:
		t.Fatal("first Elasticsearch endpoint received request, want selected second endpoint only")
	default:
	}
}

func TestSendDiscoversOlderElasticsearchVersion(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"6.8.23"}}`))
		case "/_bulk":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read bulk body: %v", err)
			}
			received <- string(body)
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":false}`))
		default:
			t.Fatalf("path = %q, want / or /_bulk", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `"_type":"_doc"`) {
			t.Fatalf("bulk body = %q, want _type _doc for Elasticsearch 6", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerResolvesIndexTimeAndApisixVariables(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
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
		Field:         FieldConfig{Index: "apisix-$route_id-{%Y}"},
		LogFormat:     map[string]string{"path": "$uri"},
		Timeout:       10,
		BatchMaxSize:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{"$route_id": "route-1"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		wantIndex := `"apisix-route-1-` + time.Now().Format("2006") + `"`
		if !strings.Contains(body, `"_index":`+wantIndex) {
			t.Fatalf("bulk body = %q, want resolved index containing %s", body, wantIndex)
		}
		if strings.Contains(body, elasticsearchIndexField) {
			t.Fatalf("bulk body = %q, want internal index field omitted from document", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
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
		EndpointAddrs:    []string{server.URL},
		Field:            FieldConfig{Index: "apisix-logs"},
		Timeout:          10,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	select {
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("document request body = %#v, want original request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("document response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
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
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("document request body = %#v, want captured request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("document response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
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
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		document := extractBulkDocument(t, body)
		if _, ok := document["request"]; ok {
			t.Fatalf("document request = %#v, want no request body", document["request"])
		}
		if _, ok := document["response"]; ok {
			t.Fatalf("document response = %#v, want no response body", document["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":          "http://127.0.0.1:9200",
		"field":                  map[string]any{"index": "apisix"},
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsEndpointAddrHeadersAndBodyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":       "http://127.0.0.1:9200",
		"field":               map[string]any{"index": "apisix"},
		"headers":             map[string]any{"X-Cluster": "logs"},
		"batch_max_size":      2,
		"max_pending_entries": 100,
		"include_req_body":    true,
		"include_resp_body":   true,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected config fields: %v", err)
	}
}

func extractBulkDocument(t *testing.T, body string) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("bulk body = %q, want action and document lines", body)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &document); err != nil {
		t.Fatalf("unmarshal bulk document: %v", err)
	}
	return document
}
