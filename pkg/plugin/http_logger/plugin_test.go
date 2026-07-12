package http_logger

import (
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

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{URI: "http://127.0.0.1/logs"})

	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want official default 3 seconds", p.config.Timeout)
	}
	if p.config.ConcatMethod != "json" {
		t.Fatalf("concat_method = %q, want json", p.config.ConcatMethod)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.MaxRetryCount != 0 {
		t.Fatalf("max_retry_count = %d, want 0", p.config.MaxRetryCount)
	}
}

func TestPostInitRejectsInvalidEncryptedAuthHeader(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	authHeader := "not-a-ciphertext"
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted auth_header rejection")
	}
}

func TestPostInitResolvesEncryptedAuthHeader(t *testing.T) {
	key := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	authHeader := encryptHTTPLoggerTestValue(t, key, "Bearer secret")
	p := &Plugin{config: Config{URI: "http://127.0.0.1/logs", AuthHeader: &authHeader}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.AuthHeader == nil || *p.config.AuthHeader != "Bearer secret" {
		t.Fatalf("auth_header = %v, want decrypted value", p.config.AuthHeader)
	}
}

func TestSendPostsJSONLogWithAuthorizationHeader(t *testing.T) {
	authHeader := "Bearer secret"
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != authHeader {
			t.Fatalf("authorization = %q, want %q", r.Header.Get("Authorization"), authHeader)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:        server.URL + "/logs?source=apisix",
		AuthHeader: &authHeader,
		Timeout:    3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if body["path"] != "/orders" {
			t.Fatalf("body = %#v, want path /orders", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func encryptHTTPLoggerTestValue(t *testing.T, key string, value string) string {
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

func TestPostInitSetsTextContentTypeForNewLineConcat(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:          server.URL,
		ConcatMethod: "new_line",
		Timeout:      3,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case got := <-received:
		if got != "text/plain" {
			t.Fatalf("content-type = %q, want text/plain", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerBatchesJSONLogs(t *testing.T) {
	received := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		if len(body) != 2 {
			t.Fatalf("batch length = %d, want 2", len(body))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerBatchesNewLineLogs(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:             server.URL,
		ConcatMethod:    "new_line",
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("body = %q, want two newline-delimited JSON entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("line %q is not JSON: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched http log request")
	}
}

func TestHandlerDropsWhenMaxPendingEntriesExceeded(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:               server.URL,
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   60,
		BufferDuration:    60,
	})
	t.Cleanup(func() {
		close(release)
		p.BatchProcessor.Stop()
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/three", nil))

	stats := p.BatchProcessor.Stats()
	if stats.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", stats.Dropped)
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:              server.URL,
		BatchMaxSize:     1,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Body.String() != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream body preserved", rr.Body.String())
	}
	select {
	case body := <-upstreamBody:
		if body != `{"order":1}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		request, ok := body["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", body["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := body["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", body["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		URI:                 server.URL,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	upstreamBody := make(chan string, 1)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", strings.NewReader(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream read body: %v", err)
		}
		upstreamBody <- string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-upstreamBody:
		if body != `{"order":3}` {
			t.Fatalf("upstream request body = %q, want original body", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request body")
	}
	select {
	case body := <-received:
		if _, ok := body["request"]; ok {
			t.Fatalf("request = %#v, want no logged request body", body["request"])
		}
		if _, ok := body["response"]; ok {
			t.Fatalf("response = %#v, want no logged response body", body["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for http log request")
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"uri":                 "http://127.0.0.1/logs",
		"batch_max_size":      10,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     2,
		"inactive_timeout":    1,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official batch fields: %v", err)
	}
}
