package syslog

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
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
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 514})

	if p.config.Timeout != 3000 {
		t.Fatalf("timeout = %d, want official default 3000 milliseconds", p.config.Timeout)
	}
	if p.config.SockType != "tcp" {
		t.Fatalf("sock_type = %q, want tcp", p.config.SockType)
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:      host,
		Port:      mustAtoi(t, port),
		SockType:  "udp",
		Timeout:   3000,
		LogFormat: map[string]string{"path": "$uri"},
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		SockType:         "udp",
		Timeout:          3000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
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
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		SockType:            "udp",
		Timeout:             3000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
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
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		SockType:            "udp",
		Timeout:             3000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
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
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestSchemaAcceptsOfficialBodyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                   "127.0.0.1",
		"port":                   514,
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
		"max_req_body_bytes":     1024,
		"max_resp_body_bytes":    2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body fields: %v", err)
	}
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
		t.Fatalf("unmarshal syslog payload: %v", err)
	}
	return payload
}

func startUDPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _, err := conn.ReadFromUDP(buf)
		if err == nil {
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
