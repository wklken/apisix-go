package zipkin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

type capabilityResponseWriter struct {
	http.ResponseWriter
	conn    net.Conn
	flushed bool
}

func (w *capabilityResponseWriter) Flush() {
	w.flushed = true
}

func (w *capabilityResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestStatusRecorderExposesResponseWriterCapabilities(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })

	underlying := &capabilityResponseWriter{conn: server}
	recorder := &statusRecorder{ResponseWriter: underlying}

	controller := http.NewResponseController(recorder)
	if err := controller.Flush(); err != nil {
		t.Fatalf("Flush() error = %v, want delegated flush", err)
	}
	if !underlying.flushed {
		t.Fatal("Flush() did not reach the underlying writer")
	}

	conn, rw, err := controller.Hijack()
	if err != nil {
		t.Fatalf("Hijack() error = %v, want delegated hijack", err)
	}
	if conn == nil {
		t.Fatal("Hijack() returned nil connection")
	}
	if rw == nil {
		t.Fatal("Hijack() returned nil ReadWriter")
	}
}

func TestRandomHexReturnsErrorForFailingReader(t *testing.T) {
	value, err := randomHex(failingReader{}, 16)
	if err == nil {
		t.Fatal("randomHex() error = nil, want failure")
	}
	if value != "" {
		t.Fatalf("randomHex() = %q, want empty on failure", value)
	}
}

func TestRandomUnitReturnsErrorForFailingReader(t *testing.T) {
	value, err := randomUnit(failingReader{})
	if err == nil {
		t.Fatal("randomUnit() error = nil, want failure")
	}
	if value != 0 {
		t.Fatalf("randomUnit() = %v, want zero on failure", value)
	}
}

func TestDeliverSpansCancelsZipkinRequestWithContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := p.deliverSpans(ctx, []map[string]any{{"name": "apisix.request"}}, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Zipkin request")
	}
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("deliverSpans() did not return after context cancellation")
	}
	if err == nil {
		t.Fatal("deliverSpans() error = nil, want context cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deliverSpans() error = %v, want context cancellation when backend did not observe it", err)
		}
	}
}

func TestHandlerFailsClosedWhenSampleRandomUnavailable(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
		SampleRatio: 0.25,
	})
	p.sampleRandom = func() (float64, error) { return 0, errors.New("random unavailable") }

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
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
	t.Cleanup(p.Stop)

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
	p.sampleRandom = func() (float64, error) { return 0.2, nil }
	sample, err := p.shouldSample()
	if err != nil || !sample {
		t.Fatalf("sample value below sample_ratio was rejected: %t/%v", sample, err)
	}
	p.sampleRandom = func() (float64, error) { return 0.3, nil }
	sample, err = p.shouldSample()
	if err != nil || sample {
		t.Fatalf("sample value above sample_ratio was accepted: %t/%v", sample, err)
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

// newReporterTestPlugin builds a plugin whose span delivery is bounded and
// fast-failing so the async-reporter tests are deterministic.
func newReporterTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{
		config:            cfg,
		reportTimeout:     300 * time.Millisecond,
		maxPendingEntries: 2,
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestReporterAsyncDeliveryDoesNotBlockRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newReporterTestPlugin(t, Config{
		Endpoint:    server.URL,
		SampleRatio: 1,
	})

	start := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)
	elapsed := time.Since(start)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rr.Code)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("request took %v, want completion without waiting for span delivery", elapsed)
	}
}

func TestReporterQueueSaturationIsObservable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newReporterTestPlugin(t, Config{
		Endpoint:    server.URL,
		SampleRatio: 1,
	})

	for range 4 {
		p.processor.Push(map[string]any{"name": "apisix.request"})
	}

	stats := p.processor.Stats()
	if stats.Dropped < 1 {
		t.Fatalf("saturation was not observable: stats = %+v, want dropped >= 1", stats)
	}
}

func TestReporterStopDrainsOrTimesOutDeterministically(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newReporterTestPlugin(t, Config{
		Endpoint:    server.URL,
		SampleRatio: 1,
	})

	p.processor.Push(map[string]any{"name": "apisix.request"})

	start := time.Now()
	p.Stop()
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Fatalf("Stop took %v, want bounded by the report timeout", elapsed)
	}
	stats := p.processor.Stats()
	if stats.FailedDrops != 1 {
		t.Fatalf("stats = %+v, want the timed-out delivery counted as a failed drop", stats)
	}
}
