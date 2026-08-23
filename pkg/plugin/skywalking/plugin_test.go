package skywalking

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

func TestRandomIDReturnsErrorForFailingReader(t *testing.T) {
	value, err := randomID(failingReader{}, 16)
	if err == nil {
		t.Fatal("randomID() error = nil, want failure")
	}
	if value != "" {
		t.Fatalf("randomID() = %q, want empty on failure", value)
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

func TestHandlerFailsClosedWhenSampleRandomUnavailable(t *testing.T) {
	p := newTestPlugin(t, Config{SampleRatio: 0.25})
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
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitRequiresEffectiveConfig(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil || err.Error() != "effective config is required" {
		t.Fatalf("PostInit() error = %v, want stable missing-config error", err)
	}
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

func TestShouldSampleUsesFractionalRatio(t *testing.T) {
	p := newTestPlugin(t, Config{SampleRatio: 0.25})
	p.sampleRandom = func() (float64, error) { return 0.24, nil }
	sample, err := p.shouldSample()
	if err != nil || !sample {
		t.Fatalf("shouldSample() = %t/%v below sample ratio, want true/nil", sample, err)
	}

	p.sampleRandom = func() (float64, error) { return 0.25, nil }
	sample, err = p.shouldSample()
	if err != nil || sample {
		t.Fatalf("shouldSample() = %t/%v at sample ratio boundary, want false/nil", sample, err)
	}
}

func TestParseSW8Context(t *testing.T) {
	traceID := base64.RawURLEncoding.EncodeToString([]byte("trace-id"))
	segmentID := base64.RawURLEncoding.EncodeToString([]byte("segment-id"))
	parentService := base64.RawURLEncoding.EncodeToString([]byte("parent-service"))
	parentInstance := base64.RawURLEncoding.EncodeToString([]byte("parent-instance"))
	parentEndpoint := base64.RawURLEncoding.EncodeToString([]byte("parent-endpoint"))
	address := base64.RawURLEncoding.EncodeToString([]byte("gateway.example.com:80"))

	ctx, ok := parseSW8(
		"1-" + traceID + "-" + segmentID + "-7-" + parentService + "-" + parentInstance + "-" + parentEndpoint + "-" +
			address,
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
	if ctx.AddressUsedAtClient != "gateway.example.com:80" {
		t.Fatalf("address used at client = %q, want gateway.example.com:80", ctx.AddressUsedAtClient)
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
	p.Flush()

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
	address := base64.RawURLEncoding.EncodeToString([]byte("gateway.example.com:80"))

	p := newTestPlugin(t, Config{
		EndpointAddr:        server.URL,
		ServiceName:         "gateway",
		ServiceInstanceName: "instance-a",
		SampleRatio:         1,
	})

	req := httptest.NewRequest(http.MethodPost, "/pay", nil)
	req.Header.Set(
		"sw8",
		"1-"+traceID+"-"+segmentID+"-3-"+parentService+"-"+parentInstance+"-"+parentEndpoint+"-"+address,
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
	p.Flush()

	select {
	case segments := <-reported:
		segment := segments[0]
		if segment["traceId"] != "incoming-trace" {
			t.Fatalf("reported traceId = %v, want incoming-trace", segment["traceId"])
		}
		if _, ok := segment["segmentReference"]; ok {
			t.Fatalf("segmentReference must not be emitted at segment level: %#v", segment)
		}
		spans := segment["spans"].([]any)
		span := spans[0].(map[string]any)
		refs, ok := span["refs"].([]any)
		if !ok || len(refs) != 1 {
			t.Fatalf("span refs = %#v, want one cross-process reference", span["refs"])
		}
		ref := refs[0].(map[string]any)
		if ref["refType"] != "CrossProcess" || ref["parentTraceSegmentId"] != "parent-segment" ||
			ref["networkAddressUsedAtPeer"] != "gateway.example.com:80" {
			t.Fatalf("span reference = %#v, want decoded SkyWalking cross-process reference", ref)
		}
		tags, ok := span["tags"].([]any)
		if !ok || len(tags) != 3 {
			t.Fatalf("span tags = %#v, want three key/value tags", span["tags"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SkyWalking report")
	}
}

func TestReportIntervalBuffersSegmentsUntilFlush(t *testing.T) {
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

	p := newTestPlugin(t, Config{EndpointAddr: server.URL, ReportInterval: 60})
	p.reportSegment(skywalkingSegment{TraceID: "trace-a"})
	p.reportSegment(skywalkingSegment{TraceID: "trace-b"})
	select {
	case segments := <-reported:
		t.Fatalf("segments reported before interval/flush: %#v", segments)
	case <-time.After(50 * time.Millisecond):
	}

	p.Flush()
	select {
	case segments := <-reported:
		if len(segments) != 2 {
			t.Fatalf("segments len = %d, want 2", len(segments))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for buffered SkyWalking report")
	}
}

func TestNestedSkyWalkingHandlersTraceRequestOnce(t *testing.T) {
	var segments atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reported []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
			t.Fatalf("decode skywalking segments: %v", err)
		}
		segments.Add(int64(len(reported)))
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	routePlugin := newTestPlugin(t, Config{EndpointAddr: server.URL, ReportInterval: 60})
	globalPlugin := newTestPlugin(t, Config{EndpointAddr: server.URL, ReportInterval: 60})
	handler := routePlugin.Handler(globalPlugin.Handler(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		},
	)))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/once", nil))
	routePlugin.Flush()
	globalPlugin.Flush()

	if got := segments.Load(); got != 1 {
		t.Fatalf("reported segments = %d, want one trace when route and global rule both enable skywalking", got)
	}
}

func TestReportWindowBoundsQueuedSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{EndpointAddr: server.URL, ReportInterval: 60})
	for range maxPendingSkyWalkingSegments + 10 {
		p.reportSegment(skywalkingSegment{TraceID: "trace"})
	}

	p.reportMu.Lock()
	queued := len(p.segments)
	dropped := p.dropped
	p.reportMu.Unlock()

	if queued != maxPendingSkyWalkingSegments {
		t.Fatalf("queued segments = %d, want the bounded window of %d", queued, maxPendingSkyWalkingSegments)
	}
	if dropped < 10 {
		t.Fatalf("dropped segments = %d, want the overflow beyond the window counted", dropped)
	}
}

func TestFailedFlushRequeuesSegmentsForRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode skywalking segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{EndpointAddr: server.URL, ReportInterval: 60})
	p.reportSegment(skywalkingSegment{TraceID: "trace-a"})

	p.Flush()
	p.reportMu.Lock()
	queued := len(p.segments)
	p.reportMu.Unlock()
	if queued != 1 {
		t.Fatalf("queued segments after failed flush = %d, want the segment requeued for retry", queued)
	}

	fail.Store(false)
	p.Flush()
	select {
	case segments := <-reported:
		if len(segments) != 1 || segments[0]["traceId"] != "trace-a" {
			t.Fatalf("retried segments = %#v, want the requeued segment", segments)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the retried SkyWalking report")
	}
}

func TestTraceStartsAtInheritedRewriteAndEndsOnce(t *testing.T) {
	reported := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{EndpointAddr: server.URL, SampleRatio: 1, ReportInterval: 60})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
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
		time.Now(),
	)
	apisixctx.SetResponseSource(result.Request, apisixctx.ResponseSourceCacheHit)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("second lifecycle finalization failures = %#v", failures)
	}
	p.Flush()
	select {
	case segments := <-reported:
		if len(segments) != 1 {
			t.Fatalf("segments = %d, want one", len(segments))
		}
		spans, ok := segments[0]["spans"].([]any)
		if !ok || len(spans) != 1 {
			t.Fatalf("spans = %#v, want one", segments[0]["spans"])
		}
		span := spans[0].(map[string]any)
		tags, ok := span["tags"].([]any)
		if !ok || len(tags) < 4 {
			t.Fatalf("tags = %#v, want detached outcome and source tags", span["tags"])
		}
		found := map[string]string{}
		for _, value := range tags {
			tag := value.(map[string]any)
			found[tag["key"].(string)] = tag["value"].(string)
		}
		if found["apisix.response_source"] != "cache_hit" || found["apisix.request_id"] != "trace-request-1" ||
			found["apisix.outcome"] != "completed" || found["apisix.retry_count"] != "2" ||
			found["http.upstream_status_code"] != "201" {
			t.Fatalf("correlation tags = %#v", found)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lifecycle-owned segment")
	}
}

func TestRouteRewriteOwnsSkyWalkingTraceStateOverGlobal(t *testing.T) {
	reported := make(chan []map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var segments []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&segments); err != nil {
			t.Fatalf("decode segments: %v", err)
		}
		reported <- segments
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	global := newTestPlugin(t, Config{
		EndpointAddr: server.URL, ServiceName: "global-service", ReportInterval: 60,
	})
	route := newTestPlugin(t, Config{
		EndpointAddr: server.URL, ServiceName: "route-service", ReportInterval: 60,
	})
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	request.Header.Set("sw8", sw8Context{
		TraceID:        "client-trace-id",
		TraceSegmentID: "client-segment-id",
		SpanID:         7,
	}.header("client-service", "client-instance", "/client"))
	globalResult := global.RunRequestPhase(httptest.NewRecorder(), request)
	routeResult := route.RunRequestPhase(httptest.NewRecorder(), globalResult.Request)
	state, ok := routeResult.Request.Context().Value(segmentStateContextKey{}).(*segmentState)
	if !ok {
		t.Fatal("final route segment state is missing")
	}
	if state.context.ParentService != "client-service" || state.context.ParentTraceSegmentID != "client-segment-id" {
		t.Fatalf(
			"route parent = %q/%q, want original client parent",
			state.context.ParentService,
			state.context.ParentTraceSegmentID,
		)
	}
	lifecycle.SetFinalRequest(routeResult.Request)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}

	global.Flush()
	route.Flush()
	select {
	case segments := <-reported:
		if len(segments) != 1 {
			t.Fatalf("reported segments = %#v, want one route-owned segment", segments)
		}
		if got := segments[0]["service"]; got != "route-service" {
			t.Fatalf("reported service = %v, want route-service", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for route-owned SkyWalking report")
	}
	select {
	case extra := <-reported:
		t.Fatalf("reported extra segment = %#v, want only the final route binding", extra)
	default:
	}
}

func TestRouteUnsamplingRemovesGeneratedSkyWalkingHeaderAndReport(t *testing.T) {
	for _, test := range []struct {
		name        string
		originalSW8 string
		wantHeader  string
	}{
		{name: "no client header"},
		{name: "preserve client header", originalSW8: "client-skywalking-header", wantHeader: "client-skywalking-header"},
	} {
		t.Run(test.name, func(t *testing.T) {
			global := newTestPlugin(t, Config{SampleRatio: 1, ReportInterval: 60})
			route := newTestPlugin(t, Config{SampleRatio: 0.25, ReportInterval: 60})
			route.sampleRandom = func() (float64, error) { return 1, nil }
			request, lifecycle := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
			)
			if test.originalSW8 != "" {
				request.Header.Set("sw8", test.originalSW8)
			}

			globalResult := global.RunRequestPhase(httptest.NewRecorder(), request)
			routeResult := route.RunRequestPhase(httptest.NewRecorder(), globalResult.Request)
			if got := routeResult.Request.Header.Get("sw8"); got != test.wantHeader {
				t.Fatalf("final sw8 header = %q, want %q", got, test.wantHeader)
			}
			lifecycle.SetFinalRequest(routeResult.Request)
			lifecycle.Complete(
				apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
				time.Now(),
			)
			if failures := lifecycle.Finalize(); len(failures) != 0 {
				t.Fatalf("lifecycle failures = %#v", failures)
			}
			for name, plugin := range map[string]*Plugin{"global": global, "route": route} {
				plugin.reportMu.Lock()
				queued := len(plugin.segments)
				plugin.reportMu.Unlock()
				if queued != 0 {
					t.Fatalf("%s queued segments = %d, want no report after route unsampling", name, queued)
				}
			}
		})
	}
}

func TestUnsampledTraceRegistersNoExportFinalizer(t *testing.T) {
	p := newTestPlugin(t, Config{EndpointAddr: "http://127.0.0.1:12800", SampleRatio: 0.25, ReportInterval: 60})
	p.sampleRandom = func() (float64, error) { return 0.9, nil }
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
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
	p.reportMu.Lock()
	queued := len(p.segments)
	p.reportMu.Unlock()
	if queued != 0 {
		t.Fatalf("queued segments = %d, want none for unsampled start", queued)
	}
}

func TestTracerDirectHandlerDoesNotDuplicateProductionOwner(t *testing.T) {
	p := newTestPlugin(t, Config{EndpointAddr: "http://127.0.0.1:12800", SampleRatio: 1, ReportInterval: 60})
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
	p.reportMu.Lock()
	queued := len(p.segments)
	p.reportMu.Unlock()
	if queued != 0 {
		t.Fatalf("queued segments = %d, want no direct-handler duplicate", queued)
	}
}
