package zipkin

import (
	"context"
	"encoding/json"
	"net"
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

func TestPostInitSetsZipkinDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
		SampleRatio: 1,
	})

	if p.config.ServiceName != "APISIX" {
		t.Fatalf("service_name = %q, want APISIX", p.config.ServiceName)
	}
	if p.config.SpanVersion != 2 {
		t.Fatalf("span_version = %d, want 2", p.config.SpanVersion)
	}
}

func TestShouldSampleUsesConfiguredRatio(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
		SampleRatio: 0.25,
	})
	p.sampleRandom = func() float64 { return 0.2 }
	if !p.shouldSample() {
		t.Fatal("sample value below sample_ratio was rejected")
	}
	p.sampleRandom = func() float64 { return 0.3 }
	if p.shouldSample() {
		t.Fatal("sample value above sample_ratio was accepted")
	}
}

func TestParseSingleB3Header(t *testing.T) {
	ctx, err := parseSingleB3("463ac35c9f6413ad48485a3953bb6124-a2fb4a1d1a96d312-1-0020000000000001")
	if err != nil {
		t.Fatalf("parseSingleB3() error = %v", err)
	}
	if ctx.TraceID != "463ac35c9f6413ad48485a3953bb6124" {
		t.Fatalf("trace id = %q", ctx.TraceID)
	}
	if ctx.ParentSpanID != "0020000000000001" {
		t.Fatalf("parent span id = %q", ctx.ParentSpanID)
	}
	if ctx.Sampled != "1" {
		t.Fatalf("sampled = %q, want 1", ctx.Sampled)
	}
}

func TestInvalidSingleB3HeaderReturnsBadRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
		SampleRatio: 1,
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("b3", "missing-span")

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for invalid b3")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlerInjectsB3AndReportsZipkinSpan(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spans []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
			t.Fatalf("decode zipkin spans: %v", err)
		}
		reported <- spans
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Endpoint:    server.URL,
		SampleRatio: 1,
		ServiceName: "apisix-go",
		ServerAddr:  "127.0.0.1",
	})

	nextCalled := false
	req := httptest.NewRequest(http.MethodGet, "/orders?status=open", nil)
	req.RemoteAddr = "203.0.113.9:4321"
	req = req.WithContext(context.WithValue(
		req.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9080},
	))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		if r.Header.Get("x-b3-traceid") == "" {
			t.Fatal("x-b3-traceid is empty")
		}
		if r.Header.Get("x-b3-spanid") == "" {
			t.Fatal("x-b3-spanid is empty")
		}
		if r.Header.Get("x-b3-sampled") != "1" {
			t.Fatalf("x-b3-sampled = %q, want 1", r.Header.Get("x-b3-sampled"))
		}
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)

	if !nextCalled {
		t.Fatal("next handler was not called")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}

	select {
	case spans := <-reported:
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		span := spans[0]
		if span["name"] != "apisix.request" {
			t.Fatalf("span name = %v, want apisix.request", span["name"])
		}
		if span["kind"] != "SERVER" {
			t.Fatalf("span kind = %v, want SERVER", span["kind"])
		}
		if span["traceId"] == "" || span["id"] == "" {
			t.Fatalf("span trace/id missing: %#v", span)
		}
		tags, ok := span["tags"].(map[string]any)
		if !ok {
			t.Fatalf("tags = %#v, want object", span["tags"])
		}
		if tags["http.status_code"] != "201" {
			t.Fatalf("http.status_code = %v, want 201", tags["http.status_code"])
		}
		localEndpoint, ok := span["localEndpoint"].(map[string]any)
		if !ok {
			t.Fatalf("localEndpoint = %#v, want object", span["localEndpoint"])
		}
		if localEndpoint["serviceName"] != "apisix-go" {
			t.Fatalf("serviceName = %v, want apisix-go", localEndpoint["serviceName"])
		}
		if localEndpoint["port"] != float64(9080) {
			t.Fatalf("local endpoint port = %v, want 9080", localEndpoint["port"])
		}
		remoteEndpoint, ok := span["remoteEndpoint"].(map[string]any)
		if !ok || remoteEndpoint["ipv4"] != "203.0.113.9" || remoteEndpoint["port"] != float64(4321) {
			t.Fatalf("remoteEndpoint = %#v", span["remoteEndpoint"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Zipkin report")
	}
}

func TestIncomingB3CreatesChildServerSpan(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spans []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
			t.Fatalf("decode zipkin spans: %v", err)
		}
		reported <- spans
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1, ServerAddr: "127.0.0.1"})
	const traceID = "463ac35c9f6413ad48485a3953bb6124"
	const incomingSpanID = "a2fb4a1d1a96d312"
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("b3", traceID+"-"+incomingSpanID+"-1")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-b3-traceid") != traceID {
			t.Fatalf("outgoing trace ID = %q", r.Header.Get("x-b3-traceid"))
		}
		if got := r.Header.Get("x-b3-spanid"); got == incomingSpanID || got == "" {
			t.Fatalf("outgoing span ID = %q, want a new child span", got)
		}
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)

	select {
	case spans := <-reported:
		if len(spans) != 1 || spans[0]["traceId"] != traceID || spans[0]["parentId"] != incomingSpanID {
			t.Fatalf("reported child span = %#v", spans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for child span report")
	}
}

func TestB3SampledZeroSkipsReport(t *testing.T) {
	reported := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reported <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Endpoint:    server.URL,
		SampleRatio: 1,
	})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("b3", "463ac35c9f6413ad-a2fb4a1d1a96d312-0")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-b3-sampled") != "0" {
			t.Fatalf("x-b3-sampled = %q, want 0", r.Header.Get("x-b3-sampled"))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case <-reported:
		t.Fatal("unexpected Zipkin report for sampled=0")
	case <-time.After(150 * time.Millisecond):
	}
}
