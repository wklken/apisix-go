package lago

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

func TestPostInitSetsLagoDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{"http://127.0.0.1:3000"},
		Token:               "token",
		EventTransactionID:  "req-1",
		EventSubscriptionID: "sub-1",
		EventCode:           "api-call",
	})

	if p.config.EndpointURI != "/api/v1/events/batch" {
		t.Fatalf("endpoint_uri = %q, want /api/v1/events/batch", p.config.EndpointURI)
	}
	if p.config.Timeout != 3000 {
		t.Fatalf("timeout = %d, want 3000", p.config.Timeout)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatal("ssl_verify = false, want true")
	}
	if !p.keepalive() {
		t.Fatal("keepalive() = false, want true by default")
	}
	if p.config.KeepaliveTimeout != 60000 {
		t.Fatalf("keepalive_timeout = %d, want 60000", p.config.KeepaliveTimeout)
	}
	if p.config.KeepalivePool != 5 {
		t.Fatalf("keepalive_pool = %d, want 5", p.config.KeepalivePool)
	}
	if p.config.BatchMaxSize != 100 {
		t.Fatalf("batch_max_size = %d, want 100", p.config.BatchMaxSize)
	}
}

func TestBuildEventResolvesConfiguredTemplates(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{"http://127.0.0.1:3000"},
		Token:               "token",
		EventTransactionID:  "req_${request_id}",
		EventSubscriptionID: "sub_${consumer_name}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"status": "${status}",
			"tier":   "expensive",
		},
	})

	entry := p.buildEvent(map[string]any{
		"request_id":    "abc",
		"consumer_name": "alice",
		"status":        201,
	})

	if entry.TransactionID != "req_abc" {
		t.Fatalf("transaction_id = %q, want req_abc", entry.TransactionID)
	}
	if entry.ExternalSubscriptionID != "sub_alice" {
		t.Fatalf("external_subscription_id = %q, want sub_alice", entry.ExternalSubscriptionID)
	}
	if entry.Code != "api-call" {
		t.Fatalf("code = %q, want api-call", entry.Code)
	}
	if entry.Properties["status"] != "201" {
		t.Fatalf("status property = %q, want 201", entry.Properties["status"])
	}
	if entry.Properties["tier"] != "expensive" {
		t.Fatalf("tier property = %q, want expensive", entry.Properties["tier"])
	}
	if entry.Timestamp <= 0 {
		t.Fatalf("timestamp = %f, want positive Unix timestamp", entry.Timestamp)
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
		EndpointAddrs:       []string{"http://127.0.0.1:3000", "http://127.0.0.2:3000"},
		Token:               "token",
		EventTransactionID:  "req-1",
		EventSubscriptionID: "sub-1",
		EventCode:           "api-call",
	})

	if got := p.endpointURL(); got != "http://127.0.0.2:3000/api/v1/events/batch" {
		t.Fatalf("endpointURL() = %q, want selected endpoint_addrs entry", got)
	}
}

func TestSendPostsLagoBatchEvent(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "lago-token",
		EventTransactionID:  "${request_id}",
		EventSubscriptionID: "${consumer_name}",
		EventCode:           "api-call",
		Timeout:             1000,
	})

	p.Send(map[string]any{
		"request_id":    "req-1",
		"consumer_name": "sub-1",
	})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/api/v1/events/batch" {
			t.Fatalf("path = %q, want /api/v1/events/batch", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer lago-token" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago request")
	}

	select {
	case body := <-bodies:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.TransactionID != "req-1" {
			t.Fatalf("transaction_id = %q, want req-1", event.TransactionID)
		}
		if event.ExternalSubscriptionID != "sub-1" {
			t.Fatalf("external_subscription_id = %q, want sub-1", event.ExternalSubscriptionID)
		}
		if event.Code != "api-call" {
			t.Fatalf("code = %q, want api-call", event.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago body")
	}
}

func TestSendBatchPostsMultipleLagoEvents(t *testing.T) {
	bodies := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "lago-token",
		EventTransactionID:  "${request_id}",
		EventSubscriptionID: "${consumer_name}",
		EventCode:           "api-call",
		Timeout:             1000,
		BatchMaxSize:        2,
	})

	if _, err := p.SendBatch([]map[string]any{
		{"request_id": "req-1", "consumer_name": "sub-1"},
		{"request_id": "req-2", "consumer_name": "sub-2"},
	}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-bodies:
		if len(body.Events) != 2 {
			t.Fatalf("events = %d, want 2", len(body.Events))
		}
		if body.Events[0].TransactionID != "req-1" || body.Events[1].TransactionID != "req-2" {
			t.Fatalf("events = %#v, want both transaction IDs", body.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago batch request")
	}
}

func TestHandlerCapturesRequestAndResponseVariables(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "${http_x_request_id}",
		EventSubscriptionID: "${request_method}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"path":   "${uri}",
			"status": "${status}",
		},
		Timeout:      1000,
		BatchMaxSize: 1,
	})

	req := httptest.NewRequest(http.MethodPut, "/orders/1?debug=true", strings.NewReader("request"))
	req.Header.Set("X-Request-ID", "req-1")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	select {
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.TransactionID != "req-1" {
			t.Fatalf("transaction_id = %q, want req-1", event.TransactionID)
		}
		if event.ExternalSubscriptionID != http.MethodPut {
			t.Fatalf("external_subscription_id = %q, want PUT", event.ExternalSubscriptionID)
		}
		if event.Properties["path"] != "/orders/1" {
			t.Fatalf("path property = %q, want /orders/1", event.Properties["path"])
		}
		if event.Properties["status"] != "201" {
			t.Fatalf("status property = %q, want 201", event.Properties["status"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}

func TestHandlerCapturesRequestAndResponseBodies(t *testing.T) {
	requests := make(chan lagoPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body lagoPayload
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Token:               "token",
		EventTransactionID:  "${http_x_request_id}",
		EventSubscriptionID: "${request_method}",
		EventCode:           "api-call",
		EventProperties: map[string]string{
			"request_body":  "${request_body}",
			"response_body": "${response_body}",
		},
		Timeout:          1000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":1}`))
	req.Header.Set("X-Request-ID", "req-1")
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
	case body := <-requests:
		if len(body.Events) != 1 {
			t.Fatalf("events = %d, want 1", len(body.Events))
		}
		event := body.Events[0]
		if event.Properties["request_body"] != `{"order":1}` {
			t.Fatalf("request_body property = %q, want original request body", event.Properties["request_body"])
		}
		if event.Properties["response_body"] != `{"ok":true}` {
			t.Fatalf("response_body property = %q, want upstream response body", event.Properties["response_body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Lago event")
	}
}
