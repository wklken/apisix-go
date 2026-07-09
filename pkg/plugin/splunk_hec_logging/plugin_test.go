package splunk_hec_logging

import (
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

func TestPostInitSetsSplunkDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	if p.config.Endpoint.Timeout != 10 {
		t.Fatalf("timeout = %d, want 10", p.config.Endpoint.Timeout)
	}
	if p.config.Endpoint.KeepaliveTimeout != 60000 {
		t.Fatalf("keepalive timeout = %d, want 60000", p.config.Endpoint.KeepaliveTimeout)
	}
	if !p.sslVerify() {
		t.Fatal("sslVerify() = false, want true by default")
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestBuildEventUsesSplunkHECShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   "http://127.0.0.1:8088/services/collector/event",
			Token: "token",
		},
	})

	event := p.buildEvent(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if event.Source != "apache-apisix-splunk-hec-logging" {
		t.Fatalf("source = %q, want apache-apisix-splunk-hec-logging", event.Source)
	}
	if event.SourceType != "_json" {
		t.Fatalf("sourcetype = %q, want _json", event.SourceType)
	}
	if event.Host == "" {
		t.Fatal("host is empty")
	}
	if event.Event["path"] != "/orders" {
		t.Fatalf("event path = %v, want /orders", event.Event["path"])
	}
	if event.Event["status"] != 201 {
		t.Fatalf("event status = %v, want 201", event.Event["status"])
	}
	if event.Time <= 0 {
		t.Fatalf("event time = %v, want positive Unix timestamp", event.Time)
	}
}

func TestSendPostsSplunkHECEvent(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	sslVerify := false
	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:     server.URL,
			Token:   "secret-token",
			Channel: "channel-a",
			Timeout: 1,
		},
		SSLVerify: &sslVerify,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Authorization"); got != "Splunk secret-token" {
			t.Fatalf("Authorization = %q, want Splunk secret-token", got)
		}
		if got := req.Header.Get("X-Splunk-Request-Channel"); got != "channel-a" {
			t.Fatalf("X-Splunk-Request-Channel = %q, want channel-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC request")
	}

	select {
	case body := <-bodies:
		event, ok := body["event"].(map[string]any)
		if !ok {
			t.Fatalf("body event = %#v, want object", body["event"])
		}
		if event["path"] != "/orders" {
			t.Fatalf("event path = %v, want /orders", event["path"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC body")
	}
}

func TestSendBatchPostsConcatenatedSplunkHECEvents(t *testing.T) {
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

	p := newTestPlugin(t, Config{
		Endpoint: Endpoint{
			URI:   server.URL,
			Token: "secret-token",
		},
		BatchMaxSize: 2,
	})

	if _, err := p.SendBatch([]map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-bodies:
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("body = %q, want both Splunk events", body)
		}
		if strings.Contains(body, "\n") || strings.HasPrefix(body, "[") {
			t.Fatalf("body = %q, want concatenated JSON event objects", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Splunk HEC batch request")
	}
}
