package zipkin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type failingReader struct{}

func TestPostInitWarnsOnlyForInsecureEndpoint(t *testing.T) {
	for _, test := range []struct {
		name     string
		scheme   string
		wantWarn bool
	}{
		{name: "http", scheme: "http", wantWarn: true},
		{name: "https", scheme: "https"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings []string
			stop := logger.ReplaceObserver("zipkin-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "zipkin endpoint") {
					warnings = append(warnings, entry.Message)
				}
			})
			defer stop()

			newTestPlugin(t, Config{Endpoint: test.scheme + "://127.0.0.1:9411/api/v2/spans", SampleRatio: 1})

			if test.wantWarn {
				if len(warnings) != 1 || warnings[0] != "Using zipkin endpoint with no TLS is a security risk" {
					t.Fatalf("warnings = %#v, want exact insecure endpoint warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %#v, want none for TLS endpoint", warnings)
			}
		})
	}
}

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
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
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
	for _, test := range []struct {
		configured int
		want       int
	}{
		{configured: 0, want: 2},
		{configured: 1, want: 1},
		{configured: 2, want: 2},
	} {
		t.Run(fmt.Sprintf("span_version_%d", test.configured), func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
				SampleRatio: 1,
				SpanVersion: test.configured,
			})

			if p.config.ServiceName != "APISIX" {
				t.Fatalf("service_name = %q, want APISIX", p.config.ServiceName)
			}
			if p.config.SpanVersion != test.want {
				t.Fatalf("span_version = %d, want %d", p.config.SpanVersion, test.want)
			}
		})
	}
}

func TestSchemaMatchesAPISIX317ValidationMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, test := range []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name: "minimum sample ratio",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 0.00001,
			},
		},
		{
			name: "sample ratio below minimum",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": -0.1,
			},
			wantErr: true,
		},
		{
			name: "sample ratio above maximum",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 2,
			},
			wantErr: true,
		},
		{
			name: "valid server address",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 1, "server_addr": "1.2.3.4",
			},
		},
		{
			name: "invalid server address",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 1, "server_addr": "badip",
			},
			wantErr: true,
		},
		{
			name: "span version 1",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 1, "span_version": 1,
			},
		},
		{
			name: "span version 2",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 1, "span_version": 2,
			},
		},
		{
			name: "unsupported span version",
			config: map[string]any{
				"endpoint": "http://127.0.0.1", "sample_ratio": 1, "span_version": 3,
			},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatal("invalid APISIX 3.17 Zipkin configuration accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("valid APISIX 3.17 Zipkin configuration rejected: %v", err)
			}
		})
	}
}

func TestPostInitRejectsUnknownZipkinSpanVersionBeforeAllocation(t *testing.T) {
	p := &Plugin{config: Config{
		Endpoint:    "http://127.0.0.1:9411/api/v2/spans",
		SampleRatio: 1,
		SpanVersion: 3,
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "only v1 and v2 are accepted") {
		t.Fatalf("PostInit() error = %v, want unsupported span version rejection", err)
	}
	if p.client != nil || p.processor != nil || p.clientRelease != nil {
		t.Fatal("PostInit allocated Zipkin resources before rejecting the span version")
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

func TestSingleB3DebugInjectsFlagsHeader(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(collector.Close)
	p := newTestPlugin(t, Config{Endpoint: collector.URL, SampleRatio: 0.00001})

	for _, header := range []string{
		"d",
		"80f198ee56343ba864fe8b2a57d3eff7-e457b5a2e4d86bd1-d-05e3ac9a4f6e3b90",
	} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/echo", nil)
			req.Header.Set("b3", header)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("x-b3-sampled"); got != "1" {
					t.Fatalf("x-b3-sampled = %q, want 1", got)
				}
				if got := r.Header.Get("x-b3-flags"); got != "1" {
					t.Fatalf("x-b3-flags = %q, want 1", got)
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestSingleB3WithoutSamplingUsesConfiguredRatio(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(collector.Close)
	p := newTestPlugin(t, Config{Endpoint: collector.URL, SampleRatio: 1})

	const traceID = "80f198ee56343ba864fe8b2a57d3eff7"
	const incomingSpanID = "e457b5a2e4d86bd1"
	req := httptest.NewRequest(http.MethodGet, "/echo", nil)
	req.Header.Set("b3", traceID+"-"+incomingSpanID)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("b3") != "" {
			t.Fatalf("b3 = %q, want decomposed multi-header propagation", r.Header.Get("b3"))
		}
		if r.Header.Get("x-b3-traceid") != traceID || r.Header.Get("x-b3-parentspanid") != incomingSpanID {
			t.Fatalf("injected B3 identity = %#v", r.Header)
		}
		if got := r.Header.Get("x-b3-sampled"); got != "1" {
			t.Fatalf("x-b3-sampled = %q, want configured sample decision 1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
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
	p.processor.Flush()

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

func TestSpanVersion1ExportsBoundedRequestSpan(t *testing.T) {
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
		Endpoint: server.URL, SampleRatio: 1, SpanVersion: 1,
	})
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1", nil))
	p.processor.Flush()

	select {
	case spans := <-reported:
		if len(spans) != 1 || spans[0]["name"] != "apisix.request" {
			t.Fatalf("spans = %#v, want one bounded request span", spans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for span_version=1 report")
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
	p.processor.Flush()

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

func TestB3MultiHeaderSamplingOverridesAreRequestLocal(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(collector.Close)
	p := newTestPlugin(t, Config{Endpoint: collector.URL, SampleRatio: 0.5})
	p.sampleRandom = func() (float64, error) { return 0.9, nil }

	for _, test := range []struct {
		name        string
		sampled     string
		flags       string
		wantSampled string
	}{
		{name: "configured ratio rejects", wantSampled: "0"},
		{name: "sampled one", sampled: "1", wantSampled: "1"},
		{name: "sampled true", sampled: "true", wantSampled: "1"},
		{name: "sampled zero", sampled: "0", wantSampled: "0"},
		{name: "sampled false", sampled: "false", wantSampled: "0"},
		{name: "debug overrides ratio", flags: "1", wantSampled: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/echo", nil)
			if test.sampled != "" {
				req.Header.Set("x-b3-sampled", test.sampled)
			}
			if test.flags != "" {
				req.Header.Set("x-b3-flags", test.flags)
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("x-b3-sampled"); got != test.wantSampled {
					t.Fatalf("x-b3-sampled = %q, want %q", got, test.wantSampled)
				}
				if r.Header.Get("x-b3-traceid") == "" || r.Header.Get("x-b3-spanid") == "" {
					t.Fatal("injected B3 trace/span ID is empty")
				}
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestSpanTagsReportUpstreamFailure(t *testing.T) {
	p := &Plugin{config: Config{ServiceName: "APISIX", ServerAddr: "127.0.0.1"}}
	req := apisixctx.WithRequestVars(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders?status=open", nil),
	)
	apisixctx.RegisterRequestVar(req, "$upstream_status", http.StatusServiceUnavailable)

	span := p.buildSpanWithSource(
		b3Context{TraceID: "463ac35c9f6413ad", SpanID: "a2fb4a1d1a96d312"},
		req,
		http.StatusBadGateway,
		time.Unix(1_700_000_000, 0),
		3*time.Millisecond,
		apisixctx.ResponseSourceUpstream,
	)
	tags, ok := span["tags"].(map[string]string)
	if !ok {
		t.Fatalf("tags = %#v, want string map", span["tags"])
	}
	wantTags := map[string]string{
		"component":              "apisix",
		"http.method":            http.MethodGet,
		"http.url":               "/orders?status=open",
		"http.status_code":       "502",
		"error":                  "true",
		"apisix.response_source": "upstream",
	}
	if !reflect.DeepEqual(tags, wantTags) {
		t.Fatalf("tags = %#v, want APISIX 3.17 tags %#v", tags, wantTags)
	}
}

func TestDeliverSpansReportsCollectorHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1})

	_, err := p.deliverSpans(context.Background(), []map[string]any{{"name": "apisix.request"}}, 1)
	if err == nil || err.Error() != "zipkin endpoint returned status code [503]" {
		t.Fatalf("deliverSpans() error = %v, want collector status failure", err)
	}
}

func TestReporterFlushExportsPendingSpansAsOneBatch(t *testing.T) {
	type collectorRequest struct {
		contentType string
		spans       []map[string]any
		err         error
	}
	reported := make(chan collectorRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := collectorRequest{contentType: r.Header.Get("Content-Type")}
		result.err = json.NewDecoder(r.Body).Decode(&result.spans)
		reported <- result
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1})
	for _, path := range []string{"/orders/1", "/orders/2"} {
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204", path, rr.Code)
		}
	}
	p.processor.Flush()

	select {
	case request := <-reported:
		if request.err != nil {
			t.Fatalf("decode collector request: %v", request.err)
		}
		if request.contentType != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", request.contentType)
		}
		if len(request.spans) != 2 {
			t.Fatalf("exported spans = %d, want one batch containing 2", len(request.spans))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for flushed Zipkin batch")
	}
}

func TestReporterFlushesAtPendingCapacityWithoutManualFlush(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spans []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
			t.Errorf("decode collector request: %v", err)
		}
		reported <- spans
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1})

	for i := range defaultMaxPendingEntries {
		if !p.processor.Push(map[string]any{"id": i}) {
			t.Fatalf("Push(%d) rejected before reaching pending capacity", i)
		}
	}

	select {
	case spans := <-reported:
		if len(spans) != defaultMaxPendingEntries {
			t.Fatalf("exported spans = %d, want capacity batch %d", len(spans), defaultMaxPendingEntries)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reporter did not flush automatically at pending capacity")
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
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
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
	processor := p.processor

	start := time.Now()
	p.Stop()
	elapsed := time.Since(start)

	if elapsed >= 2*time.Second {
		t.Fatalf("Stop took %v, want bounded by the report timeout", elapsed)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	stats := processor.Stats()
	if stats.FailedDrops != 1 {
		t.Fatalf("stats = %+v, want the timed-out delivery counted as a failed drop", stats)
	}
}

func TestTraceUsesRequestLifetimeAndEndsOnce(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spans []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
			t.Fatalf("decode spans: %v", err)
		}
		reported <- spans
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{Endpoint: server.URL, SampleRatio: 1})
	started := time.Unix(1_700_000_000, 123_456_000)
	finished := started.Add(2500 * time.Microsecond)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), started,
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("request phase decision = %d, want continue", result.Decision)
	}
	result.Request.Header.Set("X-Request-Id", "trace-request-1")
	apisixctx.RegisterRequestVar(result.Request, "$retry_count", 2)
	apisixctx.RegisterRequestVar(result.Request, "$upstream_status", http.StatusCreated)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusCreated},
		finished,
	)
	apisixctx.SetResponseSource(result.Request, apisixctx.ResponseSourceCacheHit)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("second lifecycle finalization failures = %#v", failures)
	}
	p.processor.Flush()
	select {
	case spans := <-reported:
		if len(spans) != 1 || spans[0]["name"] != "apisix.request" {
			t.Fatalf("spans = %#v, want one request span", spans)
		}
		if spans[0]["timestamp"] != float64(started.UnixNano()/int64(time.Microsecond)) {
			t.Fatalf("timestamp = %v, want request arrival %d", spans[0]["timestamp"], started.UnixMicro())
		}
		if spans[0]["duration"] != float64(2500) {
			t.Fatalf("duration = %v, want 2500 microseconds", spans[0]["duration"])
		}
		tags, ok := spans[0]["tags"].(map[string]any)
		if !ok || tags["http.status_code"] != "201" {
			t.Fatalf("tags = %#v, want final status 201", spans[0]["tags"])
		}
		wantTags := map[string]any{
			"component":              "apisix",
			"http.method":            http.MethodGet,
			"http.url":               "/orders",
			"http.status_code":       "201",
			"apisix.response_source": string(apisixctx.ResponseSourceCacheHit),
		}
		if !reflect.DeepEqual(tags, wantTags) {
			t.Fatalf("tags = %#v, want APISIX 3.17 tags %#v", tags, wantTags)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lifecycle-owned span")
	}
}

func TestUnsampledTraceRegistersNoExportFinalizer(t *testing.T) {
	p := newTestPlugin(t, Config{Endpoint: "http://127.0.0.1:9411/api/v2/spans", SampleRatio: 1})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	request.Header.Set("b3", "463ac35c9f6413ad-a2fb4a1d1a96d312-0")
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("request phase decision = %d, want continue", result.Decision)
	}
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	if stats := p.processor.Stats(); stats.Pending != 0 || stats.Buffered != 0 || stats.Processing != 0 {
		t.Fatalf("processor stats = %+v, want no accepted export for sampled=0", stats)
	}
}

func TestTracerDirectHandlerDoesNotDuplicateProductionOwner(t *testing.T) {
	p := newTestPlugin(t, Config{Endpoint: "http://127.0.0.1:9411/api/v2/spans", SampleRatio: 1})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).
		ServeHTTP(
			httptest.NewRecorder(), request,
		)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	if stats := p.processor.Stats(); stats.Pending != 0 || stats.Buffered != 0 || stats.Processing != 0 {
		t.Fatalf("processor stats = %+v, want no direct-handler duplicate", stats)
	}
}
