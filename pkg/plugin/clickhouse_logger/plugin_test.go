package clickhouse_logger

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
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

func TestPostInitSetsClickHouseDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          "default",
		Password:      "secret",
		Database:      "default",
		LogTable:      "apisix_logs",
	})

	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
}

func TestPostInitRejectsInvalidEncryptedPassword(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          "default",
		Password:      "not-a-ciphertext",
		Database:      "default",
		LogTable:      "apisix_logs",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted password rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          "default",
		Password:      encryptClickHouseTestValue(t, oldKey, "clickhouse-secret"),
		Database:      "default",
		LogTable:      "apisix_logs",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.Password != "clickhouse-secret" {
		t.Fatalf("password = %q, want resolved plaintext", p.config.Password)
	}
}

func TestBuildInsertBodyUsesJSONEachRow(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          "default",
		Password:      "secret",
		Database:      "default",
		LogTable:      "apisix_logs",
	})

	body := p.buildInsertBody([]map[string]any{{
		"path":   "/orders",
		"status": 201,
	}}, 1)

	if !strings.HasPrefix(body, "INSERT INTO apisix_logs FORMAT JSONEachRow ") {
		t.Fatalf("body = %q, want ClickHouse INSERT JSONEachRow prefix", body)
	}
	if !strings.Contains(body, `"path":"/orders"`) {
		t.Fatalf("body = %q, want JSON log entry", body)
	}
}

func TestEndpointURLPrefersDeprecatedEndpointAddr(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr:  "http://127.0.0.1:8123",
		EndpointAddrs: []string{"http://127.0.0.2:8123"},
		User:          "default",
		Password:      "secret",
		Database:      "default",
		LogTable:      "apisix_logs",
	})

	if got := p.endpointURL(); got != "http://127.0.0.1:8123" {
		t.Fatalf("endpointURL() = %q, want endpoint_addr", got)
	}
}

func TestEndpointURLSelectsFromEndpointAddrs(t *testing.T) {
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
		EndpointAddrs: []string{"http://127.0.0.1:8123", "http://127.0.0.2:8123"},
		User:          "default",
		Password:      "secret",
		Database:      "default",
		LogTable:      "apisix_logs",
	})

	if got := p.endpointURL(); got != "http://127.0.0.2:8123" {
		t.Fatalf("endpointURL() = %q, want selected endpoint_addrs entry", got)
	}
}

func TestSendPostsClickHouseInsert(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests <- r
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		User:          "default",
		Password:      "secret",
		Database:      "analytics",
		LogTable:      "apisix_logs",
		Timeout:       1,
		SSLVerify:     &sslVerify,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := req.Header.Get("X-ClickHouse-User"); got != "default" {
			t.Fatalf("X-ClickHouse-User = %q, want default", got)
		}
		if got := req.Header.Get("X-ClickHouse-Key"); got != "secret" {
			t.Fatalf("X-ClickHouse-Key = %q, want secret", got)
		}
		if got := req.Header.Get("X-ClickHouse-Database"); got != "analytics" {
			t.Fatalf("X-ClickHouse-Database = %q, want analytics", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ClickHouse request")
	}

	select {
	case body := <-bodies:
		if !strings.HasPrefix(body, "INSERT INTO apisix_logs FORMAT JSONEachRow ") {
			t.Fatalf("body = %q, want ClickHouse INSERT JSONEachRow prefix", body)
		}
		if !strings.Contains(body, `"path":"/orders"`) {
			t.Fatalf("body = %q, want JSON log entry", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ClickHouse body")
	}
}

func TestHandlerBatchesClickHouseRows(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		User:          "default",
		Password:      "secret",
		Database:      "analytics",
		LogTable:      "apisix_logs",
		Timeout:       1,
		SSLVerify:     &sslVerify,
		BatchMaxSize:  2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case body := <-bodies:
		prefix := "INSERT INTO apisix_logs FORMAT JSONEachRow "
		if !strings.HasPrefix(body, prefix) {
			t.Fatalf("body = %q, want ClickHouse INSERT JSONEachRow prefix", body)
		}
		rows := strings.Split(strings.TrimPrefix(body, prefix), " ")
		if len(rows) != 2 {
			t.Fatalf("rows = %v, want two JSONEachRow entries", rows)
		}
		for _, row := range rows {
			var entry map[string]any
			if err := json.Unmarshal([]byte(row), &entry); err != nil {
				t.Fatalf("unmarshal row %q: %v", row, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched ClickHouse body")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		EndpointAddrs:    []string{server.URL},
		User:             "default",
		Password:         "secret",
		Database:         "analytics",
		LogTable:         "apisix_logs",
		Timeout:          1,
		SSLVerify:        &sslVerify,
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
	case body := <-bodies:
		payload := strings.TrimPrefix(body, "INSERT INTO apisix_logs FORMAT JSONEachRow ")
		var logEntry map[string]any
		if err := json.Unmarshal([]byte(payload), &logEntry); err != nil {
			t.Fatalf("unmarshal clickhouse payload %q: %v", payload, err)
		}

		request, ok := logEntry["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", logEntry["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("payload request body = %#v, want original request body", request["body"])
		}

		response, ok := logEntry["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", logEntry["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ClickHouse body")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		User:                "default",
		Password:            "secret",
		Database:            "analytics",
		LogTable:            "apisix_logs",
		Timeout:             1,
		SSLVerify:           &sslVerify,
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
	case body := <-bodies:
		payload := strings.TrimPrefix(body, "INSERT INTO apisix_logs FORMAT JSONEachRow ")
		var logEntry map[string]any
		if err := json.Unmarshal([]byte(payload), &logEntry); err != nil {
			t.Fatalf("unmarshal clickhouse payload %q: %v", payload, err)
		}

		request, ok := logEntry["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", logEntry["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("payload request body = %#v, want captured request body", request["body"])
		}

		response, ok := logEntry["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", logEntry["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("payload response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ClickHouse body")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	bodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		User:                "default",
		Password:            "secret",
		Database:            "analytics",
		LogTable:            "apisix_logs",
		Timeout:             1,
		SSLVerify:           &sslVerify,
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
	case body := <-bodies:
		payload := strings.TrimPrefix(body, "INSERT INTO apisix_logs FORMAT JSONEachRow ")
		var logEntry map[string]any
		if err := json.Unmarshal([]byte(payload), &logEntry); err != nil {
			t.Fatalf("unmarshal clickhouse payload %q: %v", payload, err)
		}
		if _, ok := logEntry["request"]; ok {
			t.Fatalf("payload request = %#v, want no request body", logEntry["request"])
		}
		if _, ok := logEntry["response"]; ok {
			t.Fatalf("payload response = %#v, want no response body", logEntry["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ClickHouse body")
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addrs":      []any{"http://127.0.0.1:8123"},
		"user":                "default",
		"password":            "secret",
		"database":            "analytics",
		"logtable":            "apisix_logs",
		"batch_max_size":      2,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     60,
		"inactive_timeout":    5,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch and max pending fields: %v", err)
	}
}

func encryptClickHouseTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
