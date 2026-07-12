package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

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
			AdditionalAttributes:             []string{"request_method", "uri", "missing_var"},
			AdditionalHeaderPrefixAttributes: []string{"x-tenant", "x-extra-*", "x-missing"},
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/orders?debug=1", nil)
	req.Header.Set("X-Tenant", "blue")
	req.Header.Set("X-Extra-A", "1")
	req.Header.Set("X-Extra-B", "2")

	attrs := p.additionalSpanAttributes(req)
	got := map[string]string{}
	for _, attr := range attrs {
		got[string(attr.Key)] = attr.Value.AsString()
	}

	want := map[string]string{
		"request_method": "POST",
		"uri":            "/orders",
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

	hashedA, _ := (requestIDGenerator{}).NewIDs(
		context.WithValue(context.Background(), requestIDContextKey{}, "request-id"),
	)
	hashedB, _ := (requestIDGenerator{}).NewIDs(
		context.WithValue(context.Background(), requestIDContextKey{}, "request-id"),
	)
	if hashedA != hashedB || !hashedA.IsValid() {
		t.Fatalf("hashed trace IDs = %s and %s, want equal valid IDs", hashedA, hashedB)
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
