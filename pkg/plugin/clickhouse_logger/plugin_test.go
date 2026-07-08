package clickhouse_logger

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
}

func TestBuildInsertBodyUsesJSONEachRow(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:8123"},
		User:          "default",
		Password:      "secret",
		Database:      "default",
		LogTable:      "apisix_logs",
	})

	body := p.buildInsertBody(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

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
