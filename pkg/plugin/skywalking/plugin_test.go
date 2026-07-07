package skywalking

import (
	"encoding/base64"
	"encoding/json"
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

func TestPostInitSetsSkyWalkingDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.SampleRatio != 1 {
		t.Fatalf("sample_ratio = %v, want 1", p.config.SampleRatio)
	}
	if p.config.ServiceName != "APISIX" {
		t.Fatalf("service_name = %q, want APISIX", p.config.ServiceName)
	}
	if p.config.ServiceInstanceName != "APISIX Instance Name" {
		t.Fatalf("service_instance_name = %q, want APISIX Instance Name", p.config.ServiceInstanceName)
	}
	if p.config.EndpointAddr != "http://127.0.0.1:12800" {
		t.Fatalf("endpoint_addr = %q, want default OAP endpoint", p.config.EndpointAddr)
	}
	if p.config.ReportInterval != 3 {
		t.Fatalf("report_interval = %d, want 3", p.config.ReportInterval)
	}
}

func TestParseSW8Context(t *testing.T) {
	traceID := base64.RawURLEncoding.EncodeToString([]byte("trace-id"))
	segmentID := base64.RawURLEncoding.EncodeToString([]byte("segment-id"))
	parentService := base64.RawURLEncoding.EncodeToString([]byte("parent-service"))
	parentInstance := base64.RawURLEncoding.EncodeToString([]byte("parent-instance"))
	parentEndpoint := base64.RawURLEncoding.EncodeToString([]byte("parent-endpoint"))

	ctx, ok := parseSW8(
		"1-" + traceID + "-" + segmentID + "-7-" + parentService + "-" + parentInstance + "-" + parentEndpoint + "-ipport",
	)
	if !ok {
		t.Fatal("parseSW8() ok = false, want true")
	}
	if ctx.TraceID != "trace-id" || ctx.ParentTraceSegmentID != "segment-id" || ctx.ParentSpanID != 7 {
		t.Fatalf("parsed context = %#v", ctx)
	}
	if ctx.ParentService != "parent-service" || ctx.ParentEndpoint != "parent-endpoint" {
		t.Fatalf("parsed parent = %#v", ctx)
	}
}

func TestHandlerInjectsSW8AndReportsSegment(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/segments" {
			t.Fatalf("path = %q, want /v3/segments", r.URL.Path)
		}
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode skywalking segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		SampleRatio:         1,
	})

	req := httptest.NewRequest(http.MethodGet, "/orders?status=open", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw8 := r.Header.Get("sw8")
		if sw8 == "" {
			t.Fatal("sw8 header is empty")
		}
		if parts := strings.Split(sw8, "-"); len(parts) != 8 {
			t.Fatalf("sw8 parts = %d, want 8: %q", len(parts), sw8)
		}
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}

	select {
	case segments := <-reported:
		if len(segments) != 1 {
			t.Fatalf("segments len = %d, want 1", len(segments))
		}
		segment := segments[0]
		if segment["service"] != "gateway" || segment["serviceInstance"] != "instance-a" {
			t.Fatalf("segment identity = %#v", segment)
		}
		if segment["traceId"] == "" || segment["traceSegmentId"] == "" {
			t.Fatalf("segment trace IDs missing: %#v", segment)
		}
		spans, ok := segment["spans"].([]any)
		if !ok || len(spans) != 1 {
			t.Fatalf("spans = %#v, want one span", segment["spans"])
		}
		span := spans[0].(map[string]any)
		if span["operationName"] != "GET /orders" {
			t.Fatalf("operationName = %v, want GET /orders", span["operationName"])
		}
		if span["componentId"] != float64(6002) {
			t.Fatalf("componentId = %v, want 6002", span["componentId"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking report")
	}
}

func TestHandlerKeepsIncomingTraceIDInSW8(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode skywalking segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	traceID := base64.RawURLEncoding.EncodeToString([]byte("incoming-trace"))
	segmentID := base64.RawURLEncoding.EncodeToString([]byte("parent-segment"))
	parentService := base64.RawURLEncoding.EncodeToString([]byte("parent-service"))
	parentInstance := base64.RawURLEncoding.EncodeToString([]byte("parent-instance"))
	parentEndpoint := base64.RawURLEncoding.EncodeToString([]byte("parent-endpoint"))

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		SampleRatio:         1,
	})

	req := httptest.NewRequest(http.MethodPost, "/pay", nil)
	req.Header.Set(
		"sw8",
		"1-"+traceID+"-"+segmentID+"-3-"+parentService+"-"+parentInstance+"-"+parentEndpoint+"-ipport",
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, ok := parseSW8(r.Header.Get("sw8"))
		if !ok {
			t.Fatal("injected sw8 could not be parsed")
		}
		if ctx.TraceID != "incoming-trace" {
			t.Fatalf("trace id = %q, want incoming-trace", ctx.TraceID)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case segments := <-reported:
		segment := segments[0]
		if segment["traceId"] != "incoming-trace" {
			t.Fatalf("reported traceId = %v, want incoming-trace", segment["traceId"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking report")
	}
}
