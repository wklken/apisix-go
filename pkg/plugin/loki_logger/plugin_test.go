package loki_logger

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		EndpointAddrs: []string{"http://127.0.0.1:3100", "http://127.0.0.2:3100"},
	})

	if got := p.endpointURL(); got != "http://127.0.0.2:3100/loki/api/v1/push" {
		t.Fatalf("endpointURL() = %q, want selected endpoint_addrs entry", got)
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

func TestHandlerBatchesLokiValues(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Timeout:       1000,
		BatchMaxSize:  2,
		LogLabels: map[string]string{
			"job": "apisix",
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case body := <-bodies:
		streams, ok := body["streams"].([]any)
		if !ok || len(streams) != 1 {
			t.Fatalf("streams = %#v, want one stream", body["streams"])
		}
		stream, ok := streams[0].(map[string]any)
		if !ok {
			t.Fatalf("stream = %#v, want object", streams[0])
		}
		values, ok := stream["values"].([]any)
		if !ok || len(values) != 2 {
			t.Fatalf("values = %#v, want two batched values", stream["values"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched Loki body")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:    []string{server.URL},
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
	case body := <-bodies:
		entry := extractLokiEntry(t, body)
		request, ok := entry["request"].(map[string]any)
		if !ok {
			t.Fatalf("entry request = %#v, want object", entry["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("entry request body = %#v, want original request body", request["body"])
		}

		response, ok := entry["response"].(map[string]any)
		if !ok {
			t.Fatalf("entry response = %#v, want object", entry["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("entry response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
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
	case body := <-bodies:
		entry := extractLokiEntry(t, body)
		request, ok := entry["request"].(map[string]any)
		if !ok {
			t.Fatalf("entry request = %#v, want object", entry["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("entry request body = %#v, want captured request body", request["body"])
		}

		response, ok := entry["response"].(map[string]any)
		if !ok {
			t.Fatalf("entry response = %#v, want object", entry["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("entry response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
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
	case body := <-bodies:
		entry := extractLokiEntry(t, body)
		if _, ok := entry["request"]; ok {
			t.Fatalf("entry request = %#v, want no request body", entry["request"])
		}
		if _, ok := entry["response"]; ok {
			t.Fatalf("entry response = %#v, want no response body", entry["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addrs":         []any{"http://127.0.0.1:3100"},
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addrs":      []any{"http://127.0.0.1:3100"},
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

func extractLokiEntry(t *testing.T, body map[string]any) map[string]any {
	t.Helper()

	streams, ok := body["streams"].([]any)
	if !ok || len(streams) != 1 {
		t.Fatalf("streams = %#v, want one stream", body["streams"])
	}
	stream, ok := streams[0].(map[string]any)
	if !ok {
		t.Fatalf("stream = %#v, want object", streams[0])
	}
	values, ok := stream["values"].([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("values = %#v, want one value", stream["values"])
	}
	value, ok := values[0].([]any)
	if !ok || len(value) != 2 {
		t.Fatalf("value = %#v, want timestamp and entry", values[0])
	}
	entryText, ok := value[1].(string)
	if !ok {
		t.Fatalf("entry text = %#v, want string", value[1])
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(entryText), &entry); err != nil {
		t.Fatalf("decode Loki entry: %v", err)
	}
	return entry
}
