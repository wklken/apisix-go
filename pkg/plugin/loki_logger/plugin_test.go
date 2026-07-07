package loki_logger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestPostInitSetsLokiDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:3100"},
	})

	if p.config.EndpointURI != "/loki/api/v1/push" {
		t.Fatalf("endpoint_uri = %q, want /loki/api/v1/push", p.config.EndpointURI)
	}
	if p.config.TenantID != "fake" {
		t.Fatalf("tenant_id = %q, want fake", p.config.TenantID)
	}
	if p.config.Timeout != 3000 {
		t.Fatalf("timeout = %d, want 3000", p.config.Timeout)
	}
	if !p.keepalive() {
		t.Fatal("keepalive() = false, want true by default")
	}
	if got := p.config.LogLabels["job"]; got != "apisix" {
		t.Fatalf("log_labels.job = %q, want apisix", got)
	}
}

func TestBuildPayloadUsesLokiStreamShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:3100"},
		LogLabels: map[string]string{
			"job":    "apisix",
			"status": "$status",
		},
	})

	payload := p.buildPayload(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if len(payload.Streams) != 1 {
		t.Fatalf("streams = %d, want 1", len(payload.Streams))
	}
	stream := payload.Streams[0]
	if stream.Stream["job"] != "apisix" {
		t.Fatalf("stream job = %q, want apisix", stream.Stream["job"])
	}
	if stream.Stream["status"] != "201" {
		t.Fatalf("stream status = %q, want 201", stream.Stream["status"])
	}
	if len(stream.Values) != 1 {
		t.Fatalf("values = %d, want 1", len(stream.Values))
	}
	if stream.Values[0][0] == "" {
		t.Fatal("log timestamp is empty")
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(stream.Values[0][1]), &entry); err != nil {
		t.Fatalf("decode log entry: %v", err)
	}
	if entry["path"] != "/orders" {
		t.Fatalf("entry path = %v, want /orders", entry["path"])
	}
}

func TestSendPostsLokiPayload(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		TenantID:      "tenant-a",
		Headers: map[string]string{
			"X-Custom":       "custom",
			"X-Scope-OrgID":  "ignored",
			"Content-Type":   "ignored",
			"Authorization":  "Bearer token",
			"X-Another-Item": "another",
		},
		Timeout: 1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/loki/api/v1/push" {
			t.Fatalf("path = %q, want /loki/api/v1/push", req.URL.Path)
		}
		if got := req.Header.Get("X-Scope-OrgID"); got != "tenant-a" {
			t.Fatalf("X-Scope-OrgID = %q, want tenant-a", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := req.Header.Get("X-Custom"); got != "custom" {
			t.Fatalf("X-Custom = %q, want custom", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki request")
	}

	select {
	case body := <-bodies:
		streams, ok := body["streams"].([]any)
		if !ok || len(streams) != 1 {
			t.Fatalf("streams = %#v, want one stream", body["streams"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
	}
}
