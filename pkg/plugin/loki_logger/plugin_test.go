package loki_logger

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestBuildBatchPayloadGroupsEntriesByResolvedLabels(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:3100"},
		LogLabels: map[string]string{
			"service": "$http_x_service_name",
		},
	})

	payload := p.buildBatchPayload([]map[string]any{
		{"http_x_service_name": "svc-alpha", "request_headers_x_service_name": "svc-alpha"},
		{"http_x_service_name": "svc-beta", "request_headers_x_service_name": "svc-beta"},
		{"http_x_service_name": "", "request_headers_x_service_name": ""},
	})

	if len(payload.Streams) != 3 {
		t.Fatalf("streams = %d, want one stream per resolved label set", len(payload.Streams))
	}
	if got := payload.Streams[0].Stream["service"]; got != "svc-alpha" {
		t.Fatalf("first stream service = %q, want svc-alpha", got)
	}
	if got := payload.Streams[1].Stream["service"]; got != "svc-beta" {
		t.Fatalf("second stream service = %q, want svc-beta", got)
	}
	if got := payload.Streams[2].Stream["service"]; got != "" {
		t.Fatalf("third stream service = %q, want empty header value", got)
	}
}

func TestResolveLabelsLeavesMissingDynamicValuesEmpty(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:3100"},
		LogLabels: map[string]string{
			"service": "$http_x_service_name",
		},
	})

	labels := p.resolveLabels(map[string]any{})
	if got := labels["service"]; got != "" {
		t.Fatalf("missing dynamic label = %q, want empty value", got)
	}
}

func TestHandlerDefaultLogUsesPinnedRichShape(t *testing.T) {
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
		BatchMaxSize:  1,
	})
	r := httptest.NewRequest(http.MethodGet, "http://example.com/orders?status=open", nil)
	r.RemoteAddr = "192.0.2.10:4321"
	r.Header.Set("Test-Header", "only-for-test#1")
	r = apisixctx.WithApisixVars(r, map[string]string{
		"$route_id":      "loki-default",
		"$service_id":    "orders-service",
		"$consumer_name": "alice",
		"$balancer_ip":   "192.0.2.20",
		"$balancer_port": "8080",
	})
	r = apisixctx.WithRequestVars(r)
	apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(2))
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "accepted")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	})).ServeHTTP(httptest.NewRecorder(), r)

	select {
	case body := <-bodies:
		entry := extractLokiEntry(t, body)
		request := requiredObject(t, entry, "request")
		if got := request["url"]; got != "http://example.com/orders?status=open" {
			t.Fatalf("request.url = %#v, want full request URL", got)
		}
		if got := request["uri"]; got != "/orders?status=open" {
			t.Fatalf("request.uri = %#v, want request URI", got)
		}
		if got := request["method"]; got != http.MethodGet {
			t.Fatalf("request.method = %#v, want GET", got)
		}
		requestHeaders := requiredObject(t, request, "headers")
		if got := requestHeaders["test-header"]; got != "only-for-test#1" {
			t.Fatalf("request.headers.test-header = %#v, want only-for-test#1", got)
		}
		query := requiredObject(t, request, "querystring")
		if got := query["status"]; got != "open" {
			t.Fatalf("request.querystring.status = %#v, want open", got)
		}
		response := requiredObject(t, entry, "response")
		if got := response["status"]; got != float64(http.StatusAccepted) {
			t.Fatalf("response.status = %#v, want 202", got)
		}
		responseHeaders := requiredObject(t, response, "headers")
		if got := responseHeaders["x-upstream"]; got != "accepted" {
			t.Fatalf("response.headers.x-upstream = %#v, want accepted", got)
		}
		if got := response["size"]; got != float64(len("accepted")) {
			t.Fatalf("response.size = %#v, want %d", got, len("accepted"))
		}
		serverFields := requiredObject(t, entry, "server")
		if serverFields["hostname"] == "" || serverFields["version"] == "" {
			t.Fatalf("server = %#v, want hostname and version", serverFields)
		}
		if got := entry["route_id"]; got != "loki-default" {
			t.Fatalf("route_id = %#v, want loki-default", got)
		}
		if got := entry["service_id"]; got != "orders-service" {
			t.Fatalf("service_id = %#v, want orders-service", got)
		}
		consumer := requiredObject(t, entry, "consumer")
		if got := consumer["username"]; got != "alice" {
			t.Fatalf("consumer.username = %#v, want alice", got)
		}
		if got := entry["upstream"]; got != "192.0.2.20:8080" {
			t.Fatalf("upstream = %#v, want selected upstream", got)
		}
		if got := entry["client_ip"]; got != "192.0.2.10" {
			t.Fatalf("client_ip = %#v, want 192.0.2.10", got)
		}
		for _, key := range []string{"start_time", "latency", "upstream_latency", "apisix_latency"} {
			if _, ok := entry[key].(float64); !ok {
				t.Fatalf("%s = %#v, want numeric timing field", key, entry[key])
			}
		}
		if _, ok := entry["loki_log_time"]; ok {
			t.Fatal("private Loki timestamp leaked into log line")
		}
		if _, ok := entry["loki_labels"]; ok {
			t.Fatal("private Loki labels leaked into log line")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
	}
}

func TestHandlerResolvesLabelsOutsideCustomLogFormat(t *testing.T) {
	body := captureHandlerPayload(t, Config{
		LogFormat: map[string]string{"message": "fixed"},
		LogLabels: map[string]string{"service": "$http_x_service_name"},
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, func(r *http.Request) {
		r.Header.Set("X-Service-Name", "svc-alpha")
	})

	stream := extractLokiStream(t, body, 0)
	if got := streamLabels(t, stream)["service"]; got != "svc-alpha" {
		t.Fatalf("stream service = %q, want svc-alpha", got)
	}
	entry := extractLokiStreamEntry(t, stream, 0)
	if len(entry) != 1 || entry["message"] != "fixed" {
		t.Fatalf("custom log entry = %#v, want only fixed message", entry)
	}
}

func TestLogFormatExtraDoesNotClobberRichDefaults(t *testing.T) {
	body := captureHandlerPayloadWithPlugin(t, Config{}, func(p *Plugin) {
		p.logFormatExtra = map[string]string{
			"request":     "clobbered-request",
			"route_id":    "clobbered-route",
			"extra_field": "extra-value",
		}
	}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}, func(r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$route_id", "real-route")
	})

	entry := extractLokiEntry(t, body)
	request := requiredObject(t, entry, "request")
	if got := request["method"]; got != http.MethodGet {
		t.Fatalf("request.method = %#v, want GET", got)
	}
	if got := entry["route_id"]; got != "real-route" {
		t.Fatalf("route_id = %#v, want real-route", got)
	}
	if got := entry["extra_field"]; got != "extra-value" {
		t.Fatalf("extra_field = %#v, want extra-value", got)
	}
}

func TestHandlerCapturesRequestStartTimestampBeforeDownstream(t *testing.T) {
	var downstreamStarted int64
	body := captureHandlerPayload(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		downstreamStarted = time.Now().UnixNano()
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}, nil)

	stream := extractLokiStream(t, body, 0)
	values := streamValues(t, stream)
	timestamp, err := strconv.ParseInt(values[0][0].(string), 10, 64)
	if err != nil {
		t.Fatalf("parse Loki timestamp: %v", err)
	}
	if timestamp > downstreamStarted {
		t.Fatalf(
			"Loki timestamp = %d, want request start no later than downstream start %d",
			timestamp,
			downstreamStarted,
		)
	}
}

func TestBuildBatchPayloadPreservesPrivateFieldsAcrossBuilds(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:3100"},
		LogLabels:     map[string]string{"service": "$http_x_service_name"},
	})
	entry := map[string]any{
		"message":       "fixed",
		"loki_log_time": "123456789",
		"loki_labels":   map[string]string{"service": "svc-alpha"},
	}

	first := p.buildPayload(entry)
	second := p.buildPayload(entry)
	for index, payload := range []lokiPayload{first, second} {
		if len(payload.Streams) != 1 || payload.Streams[0].Stream["service"] != "svc-alpha" {
			t.Fatalf("payload %d streams = %#v, want private svc-alpha labels", index+1, payload.Streams)
		}
		if got := payload.Streams[0].Values[0][0]; got != "123456789" {
			t.Fatalf("payload %d timestamp = %q, want stable private timestamp", index+1, got)
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(payload.Streams[0].Values[0][1]), &line); err != nil {
			t.Fatalf("decode payload %d entry: %v", index+1, err)
		}
		if len(line) != 1 || line["message"] != "fixed" {
			t.Fatalf("payload %d line = %#v, want no private fields", index+1, line)
		}
	}
	if entry["loki_log_time"] != "123456789" || entry["loki_labels"] == nil {
		t.Fatalf("private fields were mutated across builds: %#v", entry)
	}
}

func TestMetadataSchemaAcceptsAdditiveLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	metadata := map[string]any{
		"log_format_extra":    map[string]any{"upstream_host": "$upstream_unresolved_host"},
		"max_pending_entries": 1,
	}
	if err := util.Validate(metadata, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected additive log format: %v", err)
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
		request := requiredObject(t, entry, "request")
		if _, ok := request["body"]; ok {
			t.Fatalf("entry request body = %#v, want absent", request["body"])
		}
		response := requiredObject(t, entry, "response")
		if _, ok := response["body"]; ok {
			t.Fatalf("entry response body = %#v, want absent", response["body"])
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

func captureHandlerPayload(
	t *testing.T,
	cfg Config,
	next http.HandlerFunc,
	prepare func(*http.Request),
) map[string]any {
	t.Helper()
	return captureHandlerPayloadWithPlugin(t, cfg, nil, next, prepare)
}

func captureHandlerPayloadWithPlugin(
	t *testing.T,
	cfg Config,
	configure func(*Plugin),
	next http.HandlerFunc,
	prepare func(*http.Request),
) map[string]any {
	t.Helper()

	bodies := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	cfg.EndpointAddrs = []string{server.URL}
	cfg.Timeout = 1000
	cfg.BatchMaxSize = 1
	p := newTestPlugin(t, cfg)
	if configure != nil {
		configure(p)
	}
	r := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	r = apisixctx.WithApisixVars(r, map[string]string{})
	r = apisixctx.WithRequestVars(r)
	if prepare != nil {
		prepare(r)
	}
	p.Handler(next).ServeHTTP(httptest.NewRecorder(), r)

	select {
	case body := <-bodies:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Loki body")
		return nil
	}
}

func requiredObject(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, object[key])
	}
	return value
}

func extractLokiStream(t *testing.T, body map[string]any, index int) map[string]any {
	t.Helper()
	streams, ok := body["streams"].([]any)
	if !ok || len(streams) <= index {
		t.Fatalf("streams = %#v, want index %d", body["streams"], index)
	}
	stream, ok := streams[index].(map[string]any)
	if !ok {
		t.Fatalf("stream %d = %#v, want object", index, streams[index])
	}
	return stream
}

func streamLabels(t *testing.T, stream map[string]any) map[string]any {
	t.Helper()
	return requiredObject(t, stream, "stream")
}

func streamValues(t *testing.T, stream map[string]any) [][]any {
	t.Helper()
	values, ok := stream["values"].([]any)
	if !ok {
		t.Fatalf("stream values = %#v, want array", stream["values"])
	}
	result := make([][]any, len(values))
	for index, value := range values {
		pair, ok := value.([]any)
		if !ok || len(pair) != 2 {
			t.Fatalf("stream value %d = %#v, want timestamp and line", index, value)
		}
		result[index] = pair
	}
	return result
}

func extractLokiStreamEntry(t *testing.T, stream map[string]any, index int) map[string]any {
	t.Helper()
	values := streamValues(t, stream)
	if len(values) <= index {
		t.Fatalf("stream values = %d, want index %d", len(values), index)
	}
	line, ok := values[index][1].(string)
	if !ok {
		t.Fatalf("stream line = %#v, want string", values[index][1])
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("decode stream entry: %v", err)
	}
	return entry
}
