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
	"os"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestPostInitAppliesBatchDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 9})

	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
}

func TestPostInitPreservesExplicitZeroRetryDelay(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"host":        "127.0.0.1",
		"port":        9,
		"retry_delay": 0,
	}, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay = %d, want explicit zero preserved", p.config.RetryDelay)
	}
}

func TestHandlerBatchesTCPLogs(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Timeout:         1000,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/one", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/two", nil))

	select {
	case message := <-received:
		var payload []map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP batch payload: %v, message=%q", err, message)
		}
		if len(payload) != 2 {
			t.Fatalf("batch length = %d, want 2", len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp batch message")
	}
}

func TestHandlerDefaultLogMatchesAPISIXFullLogShape(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
	})
	p.SetRouteContext("route-default", "127.0.0.1:9080")

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/orders?ID=1", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("X-Request", "request-value")
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "198.51.100.20")
		apisixctx.RegisterApisixVar(r, "$balancer_port", "1980")
		apisixctx.RegisterApisixVar(r, "$service_id", "service-default")
		apisixctx.RegisterApisixVar(r, "$consumer_name", "alice")
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(7))
		w.Header().Set("X-Upstream", "response-value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	assertNestedField(t, payload, "request", "url", "http://gateway.example:9080/orders?ID=1")
	assertNestedField(t, payload, "request", "method", http.MethodGet)
	assertNestedField(t, payload, "request", "uri", "/orders?ID=1")
	assertNestedNonnegativeNumber(t, payload, "request", "size")
	assertNestedField(t, payload, "response", "status", float64(http.StatusCreated))
	assertNestedNonnegativeNumber(t, payload, "response", "size")
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname() error = %v", err)
	}
	assertNestedField(t, payload, "server", "hostname", hostname)
	assertNestedField(t, payload, "server", "version", "apisix-go")
	if payload["service_id"] != "service-default" {
		t.Fatalf("service_id = %#v, want service-default", payload["service_id"])
	}
	if payload["route_id"] != "route-default" {
		t.Fatalf("route_id = %#v, want route-default", payload["route_id"])
	}
	assertNestedField(t, payload, "consumer", "username", "alice")
	if payload["client_ip"] != "192.0.2.10" {
		t.Fatalf("client_ip = %#v, want port-free address", payload["client_ip"])
	}
	if payload["upstream"] != "198.51.100.20:1980" {
		t.Fatalf("upstream = %#v, want selected upstream", payload["upstream"])
	}
	if payload["upstream_latency"] != float64(7) {
		t.Fatalf("upstream_latency = %#v, want 7", payload["upstream_latency"])
	}
	for _, field := range []string{"start_time", "latency", "apisix_latency"} {
		if _, ok := payload[field].(float64); !ok {
			t.Fatalf("%s = %#v, want numeric milliseconds", field, payload[field])
		}
	}
	requestLog := payload["request"].(map[string]any)
	requestHeaders := requestLog["headers"].(map[string]any)
	if requestHeaders["host"] != "gateway.example" {
		t.Errorf("request.headers.host = %#v, want original request host", requestHeaders["host"])
	}
	if requestHeaders["x-request"] != "request-value" {
		t.Fatalf("request.headers.x-request = %#v, want scalar request-value", requestHeaders["x-request"])
	}
	queryString := requestLog["querystring"].(map[string]any)
	if queryString["ID"] != "1" {
		t.Errorf("request.querystring.ID = %#v, want case-preserved scalar 1", queryString["ID"])
	}
	if _, ok := queryString["id"]; ok {
		t.Errorf("request.querystring.id = %#v, want uppercase key preserved", queryString["id"])
	}
	responseLog := payload["response"].(map[string]any)
	responseHeaders := responseLog["headers"].(map[string]any)
	if responseHeaders["x-upstream"] != "response-value" {
		t.Fatalf("response.headers.x-upstream = %#v, want scalar response-value", responseHeaders["x-upstream"])
	}
	for _, field := range []string{"route", "client", "timing"} {
		if _, ok := payload[field]; ok {
			t.Fatalf("%s = %#v, want APISIX flat full-log contract", field, payload[field])
		}
	}
}

func TestHandlerResolvesCustomFormatAfterDownstream(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"status":     "$status",
			"consumer":   "$consumer_name",
			"route_id":   "configured-route",
			"service_id": "configured-service",
		},
	})
	p.SetRouteContext("resolved-route", "127.0.0.1:9080")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$consumer_name", "downstream-consumer")
		apisixctx.RegisterApisixVar(r, "$service_id", "resolved-service")
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	if payload["status"] != float64(http.StatusCreated) {
		t.Fatalf("status = %#v, want downstream status", payload["status"])
	}
	if payload["consumer"] != "downstream-consumer" {
		t.Fatalf("consumer = %#v, want downstream-populated value", payload["consumer"])
	}
	if payload["route_id"] != "resolved-route" {
		t.Fatalf("route_id = %#v, want appended route context", payload["route_id"])
	}
	if payload["service_id"] != "resolved-service" {
		t.Fatalf("service_id = %#v, want appended service context", payload["service_id"])
	}
}

func TestHandlerResolvesNestedCustomFormat(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	var cfg Config
	if err := util.Parse(map[string]any{
		"host":           host,
		"port":           mustAtoi(t, port),
		"timeout":        1000,
		"batch_max_size": 1,
		"log_format": map[string]any{
			"request": map[string]any{
				"host": "$host",
				"client": map[string]any{
					"ip": "$remote_addr",
				},
			},
			"response": map[string]any{
				"status": "$status",
			},
		},
	}, &cfg); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	p := newTestPlugin(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.12:54321"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	assertNestedField(t, payload, "request", "host", "gateway.example")
	request := payload["request"].(map[string]any)
	assertNestedField(t, request, "client", "ip", "192.0.2.12")
	assertNestedField(t, payload, "response", "status", float64(http.StatusCreated))
}

func TestHandlerTruncatesCustomFormatAfterDepthFive(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"within": map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"host": "$host",
						},
					},
				},
			},
			"beyond": map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"d": map[string]any{
								"deep": "$host",
							},
						},
					},
				},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req.Host = "gateway.example"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	within := payload["within"].(map[string]any)["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)
	if within["host"] != "gateway.example" {
		t.Fatalf("depth-four host = %#v, want resolved boundary value", within["host"])
	}
	beyond := payload["beyond"].(map[string]any)["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"].(map[string]any)
	if len(beyond) != 0 {
		t.Fatalf("depth-five object = %#v, want truncated empty map", beyond)
	}
}

func TestHandlerCustomFormatOmitsAbsentServiceID(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"case":       "no-service",
			"service_id": "stale-service",
		},
	})
	p.SetRouteContext("route-without-service", "127.0.0.1:9080")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	if _, ok := payload["service_id"]; ok {
		t.Fatalf("service_id = %#v, want field omitted without service context", payload["service_id"])
	}
}

func TestSendBodyConnectionErrorIncludesDestination(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 1, Timeout: 20})

	err := p.sendBody([]byte("log"))
	if err == nil {
		t.Fatal("sendBody() error = nil, want invalid destination error")
	}
	if !strings.Contains(err.Error(), "failed to connect to TCP server: host[127.0.0.1] port[1]") {
		t.Fatalf("sendBody() error = %v, want source-compatible host and port diagnostic", err)
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		Timeout:          1000,
		BatchMaxSize:     1,
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
		BatchMaxSize:        1,
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
		BatchMaxSize:        1,
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
		requestLog, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("request = %#v, want request metadata", payload["request"])
		}
		if _, ok := requestLog["body"]; ok {
			t.Fatalf("request.body = %#v, want no logged request body", requestLog["body"])
		}
		responseLog, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("response = %#v, want response metadata", payload["response"])
		}
		if _, ok := responseLog["body"]; ok {
			t.Fatalf("response.body = %#v, want no logged response body", responseLog["body"])
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

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                "127.0.0.1",
		"port":                9000,
		"batch_max_size":      10,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     2,
		"inactive_timeout":    1,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official batch fields: %v", err)
	}
}

func TestMetadataSchemaRejectsStringLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"log_format": "'$host' '$time_iso8601'",
	}, p.GetMetadataSchema())
	if err == nil {
		t.Fatal("metadata schema accepted string log_format, want object validation error")
	}
	if !strings.Contains(err.Error(), "log_format") || !strings.Contains(err.Error(), "object") {
		t.Fatalf("metadata schema error = %v, want log_format object validation error", err)
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
	t.Cleanup(func() { _ = ln.Close() })

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
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received, serverNames
}

func acceptMessage(ln net.Listener, received chan<- string) {
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

func waitForTCPPayload(t *testing.T, received <-chan string) map[string]any {
	t.Helper()
	select {
	case message := <-received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP payload: %v", err)
		}
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TCP payload")
		return nil
	}
}

func assertNestedField(t *testing.T, payload map[string]any, object, field string, want any) {
	t.Helper()
	nested, ok := payload[object].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", object, payload[object])
	}
	if got := nested[field]; got != want {
		t.Fatalf("%s.%s = %#v, want %#v", object, field, got, want)
	}
}

func assertNestedNonnegativeNumber(t *testing.T, payload map[string]any, object, field string) {
	t.Helper()
	nested, ok := payload[object].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", object, payload[object])
	}
	value, ok := nested[field].(float64)
	if !ok || value < 0 {
		t.Fatalf("%s.%s = %#v, want nonnegative number", object, field, nested[field])
	}
}
