package tcp_logger

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestSendWritesTCPMessage(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port), Timeout: 1000})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestSendWritesTLSMessageWithServerName(t *testing.T) {
	addr, received, serverNames := startTLSServer(t)
	host, port := splitAddr(t, addr)
	serverName := "logs.example.test"

	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		TLS:        true,
		TLSOptions: &serverName,
		Timeout:    1000,
	})
	p.Send(map[string]any{"path": "/secure"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/secure"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls log message")
	}

	select {
	case got := <-serverNames:
		if got != serverName {
			t.Fatalf("SNI = %q, want %q", got, serverName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls server name")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		Timeout:          1000,
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
	case message := <-received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP log payload: %v", err)
		}
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Timeout:             1000,
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
	case message := <-received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP log payload: %v", err)
		}
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("request body = %#v, want captured request body", request["body"])
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Timeout:             1000,
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
	case message := <-received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP log payload: %v", err)
		}
		if _, ok := payload["request"]; ok {
			t.Fatalf("request = %#v, want no logged request body", payload["request"])
		}
		if _, ok := payload["response"]; ok {
			t.Fatalf("response = %#v, want no logged response body", payload["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                "127.0.0.1",
		"port":                9000,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}

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

func startTCPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received
}

func startTLSServer(t *testing.T) (string, <-chan string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	serverNames := make(chan string, 1)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- info.ServerName
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received, serverNames
}

func acceptMessage(ln net.Listener, received chan<- string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err == nil {
		received <- string(buf[:n])
	}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "logs.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"logs.example.test"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func splitAddr(t *testing.T, addr string) (string, string) {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var out int
	if _, err := fmt.Sscanf(value, "%d", &out); err != nil {
		t.Fatalf("parse int %q: %v", value, err)
	}
	return out
}
