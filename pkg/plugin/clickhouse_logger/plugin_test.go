package clickhouse_logger

import (
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
