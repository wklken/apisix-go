package loggly

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
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

func TestPostInitSetsDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{CustomerToken: "token"})

	if p.config.Severity != "INFO" {
		t.Fatalf("severity = %q, want INFO", p.config.Severity)
	}
	if len(p.config.Tags) != 1 || p.config.Tags[0] != "apisix" {
		t.Fatalf("tags = %v, want [apisix]", p.config.Tags)
	}
	if p.config.Host != "logs-01.loggly.com" {
		t.Fatalf("host = %q, want logs-01.loggly.com", p.config.Host)
	}
	if p.config.Port != 514 {
		t.Fatalf("port = %d, want 514", p.config.Port)
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

func TestPostInitRejectsInvalidEncryptedCustomerToken(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{CustomerToken: "not-a-ciphertext"}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted customer_token rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedCustomerToken(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{CustomerToken: encryptLogglyTestValue(t, oldKey, "loggly-token")}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.CustomerToken != "loggly-token" {
		t.Fatalf("customer_token = %q, want resolved plaintext", p.config.CustomerToken)
	}
}

func TestBuildMessageUsesRFC5424ShapeAndTags(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		Tags:          []string{"apisix", "route-a"},
	})

	message := p.buildMessage(map[string]any{
		"status": 200,
		"path":   "/get",
	})

	if !strings.HasPrefix(message, "<14>1 ") {
		t.Fatalf("message = %q, want INFO priority prefix <14>1", message)
	}
	if !strings.Contains(message, `[token@41058 tag="apisix" tag="route-a"]`) {
		t.Fatalf("message = %q, want Loggly structured data with tags", message)
	}
	if !strings.Contains(message, `"path":"/get"`) {
		t.Fatalf("message = %q, want JSON log payload", message)
	}
}

func TestBuildMessageUsesSeverityMap(t *testing.T) {
	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Severity:      "INFO",
		SeverityMap:   map[string]string{"503": "ERR"},
	})

	message := p.buildMessage(map[string]any{"status": 503})
	if !strings.HasPrefix(message, "<11>1 ") {
		t.Fatalf("message = %q, want ERR priority prefix <11>1", message)
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/get"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[token@41058 tag="apisix"]`) {
			t.Fatalf("message = %q, want Loggly token and default tag", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for UDP log message")
	}
}

func TestSendWritesHTTPBulkMessage(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bulk/token/tag/bulk" {
			t.Fatalf("path = %q, want /bulk/token/tag/bulk", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-LOGGLY-TAG") != "apisix,route-a" {
			t.Fatalf("X-LOGGLY-TAG = %q, want apisix,route-a", r.Header.Get("X-LOGGLY-TAG"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		received <- body["path"].(string)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Tags:          []string{"apisix", "route-a"},
		Timeout:       1000,
	})

	p.Send(map[string]any{"status": 200, "path": "/bulk"})

	select {
	case path := <-received:
		if path != "/bulk" {
			t.Fatalf("path = %q, want /bulk", path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestHandlerBatchesHTTPBulkMessages(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          server.URL,
		Protocol:      "http",
		Timeout:       1000,
		BatchMaxSize:  2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case body := <-received:
		lines := strings.Split(body, "\n")
		if len(lines) != 2 {
			t.Fatalf("bulk body = %q, want two newline-delimited entries", body)
		}
		for _, line := range lines {
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("unmarshal bulk line %q: %v", line, err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loggly HTTP bulk body")
	}
}

func TestSendBatchWritesUDPMessagesIndividually(t *testing.T) {
	addr, received := startUDPServerN(t, 2)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		CustomerToken: "token",
		Host:          host,
		Port:          mustAtoi(t, port),
		Timeout:       1000,
	})

	firstFail, err := p.SendBatch([]map[string]any{
		{"status": 200, "path": "/first"},
		{"status": 201, "path": "/second"},
	}, 2)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if firstFail != 0 {
		t.Fatalf("firstFail = %d, want 0", firstFail)
	}

	for _, want := range []string{"/first", "/second"} {
		select {
		case message := <-received:
			if !strings.Contains(message, want) {
				t.Fatalf("message = %q, want path %s", message, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for UDP log message %s", want)
		}
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:    "token",
		Host:             server.URL,
		Protocol:         "http",
		Timeout:          1000,
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
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("payload request body = %#v, want original request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
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
	case payload := <-received:
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("payload request body = %#v, want captured request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("payload response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		CustomerToken:       "token",
		Host:                server.URL,
		Protocol:            "http",
		Timeout:             1000,
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
	case payload := <-received:
		if _, ok := payload["request"]; ok {
			t.Fatalf("payload request = %#v, want no request body", payload["request"])
		}
		if _, ok := payload["response"]; ok {
			t.Fatalf("payload response = %#v, want no response body", payload["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP bulk log message")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":         "token",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBodySizeAndSSLFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
		"include_req_body":    true,
		"include_resp_body":   true,
		"ssl_verify":          false,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official config fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"customer_token":      "token",
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

func encryptLogglyTestValue(t *testing.T, key string, value string) string {
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

func startUDPServer(t *testing.T) (string, <-chan string) {
	return startUDPServerN(t, 1)
}

func startUDPServerN(t *testing.T, count int) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
	})

	received := make(chan string, count)
	go func() {
		buf := make([]byte, 4096)
		for range count {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
