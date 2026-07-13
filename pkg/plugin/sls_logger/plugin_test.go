package sls_logger

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
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

func TestPostInitSetsSLSDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	if p.config.Timeout != 5000 {
		t.Fatalf("timeout = %d, want 5000", p.config.Timeout)
	}
	if p.addr != "127.0.0.1:10009" {
		t.Fatalf("addr = %q, want 127.0.0.1:10009", p.addr)
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

func TestPostInitRejectsInvalidEncryptedAccessKeySecret(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "not-a-ciphertext",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted access_key_secret rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedAccessKeySecret(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: encryptSLSLoggerTestValue(t, oldKey, "sls-secret"),
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if p.config.AccessKeySecret != "sls-secret" {
		t.Fatalf("access_key_secret = %q, want resolved plaintext", p.config.AccessKeySecret)
	}
}

func TestBuildMessageUsesRFC5424Shape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	message := p.buildMessage(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if !strings.HasPrefix(message, "<46>1 ") {
		t.Fatalf("message = %q, want RFC5424 SYSLOG/INFO prefix <46>1", message)
	}
	wantStructured := `[logservice project="project-a" logstore="store-a" access-key-id="id" access-key-secret="secret"]`
	if !strings.Contains(message, wantStructured) {
		t.Fatalf("message = %q, want SLS structured data %s", message, wantStructured)
	}
	if !strings.Contains(message, `"path":"/orders"`) {
		t.Fatalf("message = %q, want JSON log entry", message)
	}
	if !strings.HasSuffix(message, "\n") {
		t.Fatalf("message = %q, want newline suffix", message)
	}
}

func TestSendWritesTLSMessage(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		Timeout:         1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[logservice project="project-a" logstore="store-a"`) {
			t.Fatalf("message = %q, want SLS structured data", message)
		}
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerBatchesSLSMessages(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		Timeout:         1000,
		BatchMaxSize:    2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case message := <-received:
		if got := strings.Count(message, "<46>1 "); got != 2 {
			t.Fatalf("message = %q, want two RFC5424 messages", message)
		}
		if got := strings.Count(message, "\n"); got != 2 {
			t.Fatalf("message = %q, want two newline-terminated messages", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched TLS log messages")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		Project:          "project-a",
		Logstore:         "store-a",
		AccessKeyID:      "id",
		AccessKeySecret:  "secret",
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
	case message := <-received:
		payload := extractJSONPayload(t, message)
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
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Project:             "project-a",
		Logstore:            "store-a",
		AccessKeyID:         "id",
		AccessKeySecret:     "secret",
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
	case message := <-received:
		payload := extractJSONPayload(t, message)
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
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Project:             "project-a",
		Logstore:            "store-a",
		AccessKeyID:         "id",
		AccessKeySecret:     "secret",
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
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if _, ok := payload["request"]; ok {
			t.Fatalf("payload request = %#v, want no request body", payload["request"])
		}
		if _, ok := payload["response"]; ok {
			t.Fatalf("payload response = %#v, want no response body", payload["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                   "127.0.0.1",
		"port":                   10009,
		"project":                "project-a",
		"logstore":               "store-a",
		"access_key_id":          "id",
		"access_key_secret":      "secret",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":              "127.0.0.1",
		"port":              10009,
		"project":           "project-a",
		"logstore":          "store-a",
		"access_key_id":     "id",
		"access_key_secret": "secret",
		"batch_max_size":    2,
		"max_retry_count":   1,
		"retry_delay":       1,
		"buffer_duration":   60,
		"inactive_timeout":  5,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch fields: %v", err)
	}
}

func encryptSLSLoggerTestValue(t *testing.T, key string, value string) string {
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

func extractJSONPayload(t *testing.T, message string) map[string]any {
	t.Helper()

	start := strings.Index(message, "{")
	end := strings.LastIndex(message, "}")
	if start == -1 || end == -1 || end < start {
		t.Fatalf("message = %q, want JSON payload", message)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(message[start:end+1]), &payload); err != nil {
		t.Fatalf("unmarshal SLS payload: %v", err)
	}
	return payload
}

func startTLSServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	return ln.Addr().String(), received
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return cert
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
