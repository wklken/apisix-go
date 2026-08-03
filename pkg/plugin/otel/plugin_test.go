package otel

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

type otelMinimalWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *otelMinimalWriter) Header() http.Header {
	return w.header
}

func (w *otelMinimalWriter) Write(body []byte) (int, error) {
	return w.body.Write(body)
}

func (w *otelMinimalWriter) WriteHeader(status int) {
	w.status = status
}

func TestPostInitSetsSamplerDefaults(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	if p.config.Sampler.Name != "always_off" {
		t.Fatalf("sampler name = %q, want always_off", p.config.Sampler.Name)
	}
	if p.config.Sampler.Options.Fraction != 0 {
		t.Fatalf("sampler fraction = %v, want 0", p.config.Sampler.Options.Fraction)
	}
	if p.serverName() != "APISIX" {
		t.Fatalf("server name = %q, want APISIX", p.serverName())
	}
}

func TestAdditionalSpanAttributesUseRequestVarsAndHeaders(t *testing.T) {
	p := &Plugin{
		config: Config{
			AdditionalAttributes: []string{
				"request_method",
				"uri",
				"arg_debug",
				"cookie_token",
				"missing_var",
			},
			AdditionalHeaderPrefixAttributes: []string{"x-tenant", "x-extra-*", "x-missing"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/orders?debug=1", nil)
	req.Header.Set("X-Tenant", "blue")
	req.Header.Set("X-Extra-A", "1")
	req.Header.Set("X-Extra-B", "2")
	req.AddCookie(&http.Cookie{Name: "token", Value: "auth-token"})

	attrs := p.additionalSpanAttributes(req)
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	want := map[string]string{
		"request_method": "POST",
		"uri":            "/orders",
		"arg_debug":      "1",
		"cookie_token":   "auth-token",
		"x-tenant":       "blue",
		"x-extra-a":      "1",
		"x-extra-b":      "2",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("attribute %q = %q, want %q; attrs=%v", key, got[key], value, got)
		}
	}
	for _, key := range []string{"missing_var", "x-missing"} {
		if _, ok := got[key]; ok {
			t.Fatalf("attribute %q present, want skipped; attrs=%v", key, got)
		}
	}
}

func TestHandlerAddsDownstreamAndNumericAttributesBeforeSpanEnds(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{
		config: Config{
			Sampler:              SamplerConfig{Name: "always_on"},
			AdditionalAttributes: []string{"consumer_name", "request_time", "bytes_sent"},
		},
		tracerProvider: provider,
	}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$consumer_name", "john")
		r.URL.Path = "/rewritten"
		_, _ = io.WriteString(w, "hello")
	}))
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/orders", nil)
	req.URL.RawQuery = "state=open"
	req.Header.Set("User-Agent", "otel-client")
	req = apisixctx.WithApisixVars(req, nil)
	req = apisixctx.WithRequestVars(req)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, req)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	got := make(map[string]string)
	for _, attr := range spans[0].Attributes() {
		got[string(attr.Key)] = attr.Value.String()
	}
	if got["consumer_name"] != "john" {
		t.Fatalf("consumer_name = %q, want john; attrs=%#v", got["consumer_name"], got)
	}
	if got["bytes_sent"] != "5" {
		t.Fatalf("bytes_sent = %q, want 5; attrs=%#v", got["bytes_sent"], got)
	}
	if got["request_time"] == "" || strings.HasPrefix(got["request_time"], "-") {
		t.Fatalf("request_time = %q, want non-negative duration; attrs=%#v", got["request_time"], got)
	}
	for key, want := range map[string]string{
		"http.target":     "/orders?state=open",
		"http.user_agent": "otel-client",
		"net.host.name":   "api.example.com",
	} {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; attrs=%#v", key, got[key], want, got)
		}
	}
}

func TestOTelHandlerPreservesResponseWriterCapabilities(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
	delegate := &otelMinimalWriter{header: make(http.Header)}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); ok {
			t.Error("handler advertised unsupported http.Flusher")
		}
		if _, ok := w.(http.Hijacker); ok {
			t.Error("handler advertised unsupported http.Hijacker")
		}
		if _, ok := w.(http.Pusher); ok {
			t.Error("handler advertised unsupported http.Pusher")
		}
		if _, ok := w.(io.ReaderFrom); ok {
			t.Error("handler advertised unsupported io.ReaderFrom")
		}
		_, _ = w.Write([]byte("streamed"))
	}))

	handler.ServeHTTP(delegate, httptest.NewRequest(http.MethodGet, "/", nil))

	if delegate.body.String() != "streamed" {
		t.Fatalf("body = %q, want streamed", delegate.body.String())
	}
}

func TestAdditionalSpanAttributesUseAPISIXAndRequestVars(t *testing.T) {
	p := &Plugin{
		config: Config{
			AdditionalAttributes: []string{"route_id", "service_name", "upstream_latency"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{
		"$route_id":     "route-1",
		"$service_name": "orders-service",
	})
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$upstream_latency", int64(37))

	attrs := p.additionalSpanAttributes(req)
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	want := map[string]string{
		"route_id":         "route-1",
		"service_name":     "orders-service",
		"upstream_latency": "37",
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("attribute %q = %q, want %q; attrs=%v", key, got[key], value, got)
		}
	}
}

func TestResourceContextProvidesRealChainRouteAndServiceAttributes(t *testing.T) {
	p := &Plugin{
		config: Config{AdditionalAttributes: []string{"route_id", "service_name"}},
	}
	p.SetResourceContext(
		resource.Route{ID: "route-1", Name: "orders-route", Uri: "/orders/:id", ServiceID: "service-1"},
		resource.Service{Name: "orders-service"},
	)

	req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	additional := p.additionalSpanAttributes(req)
	gotAdditional := map[string]string{}
	for _, attr := range additional {
		gotAdditional[string(attr.Key)] = attr.Value.AsString()
	}
	if gotAdditional["route_id"] != "route-1" || gotAdditional["service_name"] != "orders-service" {
		t.Fatalf("additional resource attributes = %#v, want route/service values", gotAdditional)
	}

	gotCore := map[string]string{}
	for _, attr := range p.resourceSpanAttributes() {
		gotCore[string(attr.Key)] = attr.Value.AsString()
	}
	for key, want := range map[string]string{
		"apisix.route_id":     "route-1",
		"apisix.route_name":   "orders-route",
		"http.route":          "/orders/:id",
		"apisix.service_id":   "service-1",
		"apisix.service_name": "orders-service",
	} {
		if gotCore[key] != want {
			t.Fatalf("core attribute %q = %q, want %q; attrs=%#v", key, gotCore[key], want, gotCore)
		}
	}
}

func TestBuildSamplerUsesOfficialSamplerNames(t *testing.T) {
	tests := []struct {
		name    string
		sampler SamplerConfig
		want    sdktrace.SamplingDecision
	}{
		{
			name:    "always off",
			sampler: SamplerConfig{Name: "always_off"},
			want:    sdktrace.Drop,
		},
		{
			name:    "always on",
			sampler: SamplerConfig{Name: "always_on"},
			want:    sdktrace.RecordAndSample,
		},
		{
			name: "trace ratio zero",
			sampler: SamplerConfig{
				Name:    "trace_id_ratio",
				Options: SamplerOptions{Fraction: 0},
			},
			want: sdktrace.Drop,
		},
		{
			name: "trace ratio one",
			sampler: SamplerConfig{
				Name:    "trace_id_ratio",
				Options: SamplerOptions{Fraction: 1},
			},
			want: sdktrace.RecordAndSample,
		},
		{
			name: "parent base uses configured root",
			sampler: SamplerConfig{
				Name: "parent_base",
				Options: SamplerOptions{
					Root: RootSamplerConfig{Name: "always_on"},
				},
			},
			want: sdktrace.RecordAndSample,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSampler(tt.sampler).ShouldSample(sdktrace.SamplingParameters{
				ParentContext: context.Background(),
				Name:          "GET",
			}).Decision
			if got != tt.want {
				t.Fatalf("sampling decision = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestIDGeneratorUsesXRequestIDAsTraceID(t *testing.T) {
	const requestID = "0123456789abcdef0123456789abcdef"
	ctx := context.WithValue(context.Background(), requestIDContextKey{}, requestID)

	traceID, spanID := (requestIDGenerator{}).NewIDs(ctx)
	if traceID.String() != requestID {
		t.Fatalf("trace ID = %s, want %s", traceID, requestID)
	}
	if !spanID.IsValid() {
		t.Fatalf("span ID = %s, want valid ID", spanID)
	}

	fallbackA, _ := (requestIDGenerator{}).NewIDs(
		context.WithValue(context.Background(), requestIDContextKey{}, "request-id"),
	)
	fallbackB, _ := (requestIDGenerator{}).NewIDs(
		context.WithValue(context.Background(), requestIDContextKey{}, "request-id"),
	)
	if fallbackA == fallbackB || !fallbackA.IsValid() || !fallbackB.IsValid() {
		t.Fatalf("fallback trace IDs = %s and %s, want distinct valid random IDs", fallbackA, fallbackB)
	}
}

func TestLoadMetadataUsesOfficialPluginAttributes(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = oldConfig })
	config.GlobalConfig = &config.Config{
		PluginAttr: map[string]map[string]any{
			name: {
				"trace_id_source": "x-request-id",
				"resource": map[string]any{
					"service.name": "gateway",
				},
				"collector": map[string]any{
					"address":         "collector.example.com:4318",
					"request_timeout": 7,
				},
			},
		},
	}

	metadata, configured := loadMetadata()
	if !configured {
		t.Fatal("metadata configured = false, want true")
	}
	if metadata.TraceIDSource != "x-request-id" || metadata.Collector.Address != "collector.example.com:4318" ||
		metadata.Collector.RequestTimeout != 7 || metadata.Resource["service.name"] != "gateway" {
		t.Fatalf("metadata = %#v, want configured trace source, collector, and resource", metadata)
	}
}

func TestOTelResourceRestoresDottedKeysNestedByRuntimeConfigLoader(t *testing.T) {
	resource := otelResource(map[string]any{
		"service": map[string]any{"name": "gateway"},
		"deployment": map[string]any{
			"environment": "integration",
		},
	})

	got := make(map[string]string)
	for _, attr := range resource.Attributes() {
		got[string(attr.Key)] = attr.Value.String()
	}
	if got["service.name"] != "gateway" {
		t.Fatalf("service.name = %q, want gateway; attrs=%#v", got["service.name"], got)
	}
	if got["deployment.environment"] != "integration" {
		t.Fatalf("deployment.environment = %q, want integration; attrs=%#v", got["deployment.environment"], got)
	}
}

func TestTracerProviderExportsOTLPHTTPWithConfiguredHeaders(t *testing.T) {
	requests := make(chan *http.Request, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	metadata := Metadata{
		TraceIDSource: "x-request-id",
		Resource:      map[string]any{"service.name": "gateway"},
		Collector: CollectorConfig{
			Address:        collector.URL,
			RequestTimeout: 1,
			RequestHeaders: map[string]any{"Authorization": "token"},
		},
		BatchSpanProcessor: BatchSpanProcessorConfig{
			MaxQueueSize:       8,
			BatchTimeout:       0.01,
			InactiveTimeout:    1,
			MaxExportBatchSize: 1,
		},
	}
	provider, err := newTracerProvider(SamplerConfig{Name: "always_on"}, metadata, true)
	if err != nil {
		t.Fatalf("new tracer provider: %v", err)
	}
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	p := &Plugin{
		config:         Config{Sampler: SamplerConfig{Name: "always_on"}},
		metadata:       metadata,
		tracerProvider: provider,
	}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/orders", nil)
	req.Header.Set("X-Request-ID", "0123456789abcdef0123456789abcdef")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if err := provider.ForceFlush(context.Background()); err != nil {
		t.Fatalf("force flush: %v", err)
	}

	select {
	case request := <-requests:
		if request.URL.Path != "/v1/traces" {
			t.Fatalf("collector path = %q, want /v1/traces", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "token" {
			t.Fatalf("authorization header = %q, want token", request.Header.Get("Authorization"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OTLP export")
	}
}

func TestPostInitKeepsFallbackProviderWhenCollectorIsInvalid(t *testing.T) {
	oldConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = oldConfig })
	config.GlobalConfig = &config.Config{
		PluginAttr: map[string]map[string]any{
			name: {
				"collector": map[string]any{"address": "://invalid"},
			},
		},
	}

	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid collector error")
	}
	t.Cleanup(p.Stop)
	if p.tracerProvider == nil {
		t.Fatal("fallback tracer provider = nil")
	}
}
