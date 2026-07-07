package skywalking_logger

import (
	"encoding/base64"
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

func TestPostInitSetsSkyWalkingDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "http://127.0.0.1:12800",
	})

	if p.config.ServiceName != "APISIX" {
		t.Fatalf("service_name = %q, want APISIX", p.config.ServiceName)
	}
	if p.config.ServiceInstanceName != "APISIX Instance Name" {
		t.Fatalf("service_instance_name = %q, want APISIX Instance Name", p.config.ServiceInstanceName)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
}

func TestEndpointURLAppendsLogsPath(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr: "http://127.0.0.1:12800/",
	})

	if got := p.endpointURL(); got != "http://127.0.0.1:12800/v3/logs" {
		t.Fatalf("endpointURL() = %q, want /v3/logs appended once", got)
	}
}

func TestBuildEntryUsesSkyWalkingLogShape(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddr:        "http://127.0.0.1:12800",
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		LogFormat:           map[string]string{"path": "$uri"},
		IncludeReqBody:      true,
		MaxReqBodyBytes:     128,
		MaxRespBodyBytes:    256,
		IncludeRespBody:     true,
	})

	entry := p.buildEntry(map[string]any{
		"path":                     "/orders",
		internalSkyWalkingEndpoint: "/orders",
	})

	if entry.Service != "gateway" {
		t.Fatalf("service = %q, want gateway", entry.Service)
	}
	if entry.ServiceInstance != "instance-a" {
		t.Fatalf("serviceInstance = %q, want instance-a", entry.ServiceInstance)
	}
	if entry.Endpoint != "/orders" {
		t.Fatalf("endpoint = %q, want /orders", entry.Endpoint)
	}
	if entry.Body.JSON.JSON == "" {
		t.Fatal("body.json.json is empty")
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(entry.Body.JSON.JSON), &payload); err != nil {
		t.Fatalf("decode SkyWalking json body: %v", err)
	}
	if payload["path"] != "/orders" {
		t.Fatalf("payload path = %v, want /orders", payload["path"])
	}
	if _, ok := payload[internalSkyWalkingEndpoint]; ok {
		t.Fatalf("payload includes internal endpoint marker: %#v", payload)
	}
}

func TestParseTraceContextFromSW8(t *testing.T) {
	traceID := base64.RawURLEncoding.EncodeToString([]byte("trace-id"))
	segmentID := base64.RawURLEncoding.EncodeToString([]byte("segment-id"))
	parentService := base64.RawURLEncoding.EncodeToString([]byte("parent-service"))
	parentInstance := base64.RawURLEncoding.EncodeToString([]byte("parent-instance"))
	parentEndpoint := base64.RawURLEncoding.EncodeToString([]byte("parent-endpoint"))
	trace, ok := parseTraceContext("1-" + traceID + "-" + segmentID + "-7-" + parentService + "-" + parentInstance + "-" + parentEndpoint + "-ipport")
	if !ok {
		t.Fatal("parseTraceContext() ok = false, want true")
	}
	if trace.TraceID != "trace-id" {
		t.Fatalf("traceId = %q, want trace-id", trace.TraceID)
	}
	if trace.TraceSegmentID != "segment-id" {
		t.Fatalf("traceSegmentId = %q, want segment-id", trace.TraceSegmentID)
	}
	if trace.SpanID != 7 {
		t.Fatalf("spanId = %d, want 7", trace.SpanID)
	}
}

func TestSendPostsSkyWalkingEntries(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		Timeout:             1,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case req := <-requests:
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if req.URL.Path != "/v3/logs" {
			t.Fatalf("path = %q, want /v3/logs", req.URL.Path)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking request")
	}

	select {
	case body := <-bodies:
		if len(body) != 1 {
			t.Fatalf("entries = %d, want 1", len(body))
		}
		if body[0]["service"] != "gateway" {
			t.Fatalf("service = %v, want gateway", body[0]["service"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking body")
	}
}
