package otel

import (
	"bytes"
	"context"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type failingReader struct{}

func TestPostInitWarnsOnlyForInsecureCollectorAddress(t *testing.T) {
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
			stop := logger.ReplaceObserver("opentelemetry-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "opentelemetry collector.address") {
					warnings = append(warnings, entry.Message)
				}
			})
			defer stop()

			view := mustOpenTelemetryMetadataView(t, map[string]string{
				name: `{"collector":{"address":"` + test.scheme + `://127.0.0.1:4318"}}`,
			})
			p := &Plugin{}
			p.SetDependencies(base.Dependencies{
				Config: &config.EffectiveConfig{}, Metadata: view, Tasks: newOpenTelemetryTaskOwner(t),
			})
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err != nil {
				t.Fatalf("PostInit() error = %v", err)
			}
			t.Cleanup(p.Stop)

			if test.wantWarn {
				const warning = "Using opentelemetry collector.address with no TLS is a security risk"
				if len(warnings) != 1 || warnings[0] != warning {
					t.Fatalf("warnings = %#v, want exact insecure collector warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %#v, want none for TLS collector", warnings)
			}
		})
	}
}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

type otelMinimalWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
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
	p.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{}})
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

func TestCaptureHTTPSpanAttributesMatchesAPISIX317HTTPConventions(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example.test/orders?state=open", nil)
	req.Header.Set("User-Agent", "otel-client")

	got := make(map[string]string)
	for _, attr := range captureHTTPSpanAttributes(req) {
		got[string(attr.Key)] = attr.Value.AsString()
	}
	want := map[string]string{
		"http.method":         http.MethodGet,
		"http.scheme":         "http",
		"http.target":         "/orders?state=open",
		"http.user_agent":     "otel-client",
		"http.request.method": http.MethodGet,
		"net.host.name":       "gateway.example.test",
		"url.path":            "/orders",
		"url.scheme":          "http",
		"user_agent.original": "otel-client",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP span attributes = %#v, want %#v", got, want)
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

func TestRandomTraceIDReturnsErrorForFailingReader(t *testing.T) {
	traceID, err := randomTraceID(failingReader{})
	if err == nil {
		t.Fatal("randomTraceID() error = nil, want failure")
	}
	if traceID.IsValid() {
		t.Fatalf("randomTraceID() = %s, want invalid ID on failure", traceID)
	}
}

func TestRandomSpanIDReturnsErrorForFailingReader(t *testing.T) {
	spanID, err := randomSpanID(failingReader{})
	if err == nil {
		t.Fatal("randomSpanID() error = nil, want failure")
	}
	if spanID.IsValid() {
		t.Fatalf("randomSpanID() = %s, want invalid ID on failure", spanID)
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
	view := mustOpenTelemetryMetadataView(t, map[string]string{
		name: `{"trace_id_source":"x-request-id","resource":{"service.name":"gateway"},"collector":{"address":"collector.example.com:4318","request_timeout":7}}`,
	})
	metadata, configured, err := loadMetadata(view, nil)
	if err != nil {
		t.Fatalf("loadMetadata() error = %v", err)
	}
	if !configured {
		t.Fatal("metadata configured = false, want true")
	}
	if metadata.TraceIDSource != "x-request-id" || metadata.Collector.Address != "collector.example.com:4318" ||
		metadata.Collector.RequestTimeout != 7 || metadata.Resource["service.name"] != "gateway" {
		t.Fatalf("metadata = %#v, want configured trace source, collector, and resource", metadata)
	}
}

func TestMetadataSchemaAcceptsOpenTelemetryDocument(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	metadata := map[string]any{
		"trace_id_source": "x-request-id",
		"resource": map[string]any{
			"service.name": "gateway",
			"enabled":      true,
			"sample_rate":  0.5,
		},
		"collector": map[string]any{
			"address":         "collector.example.com:4318",
			"request_timeout": -7,
			"request_headers": map[string]any{
				"Authorization": "token",
				"X-Retry":       3,
				"X-Enabled":     false,
			},
		},
		"batch_span_processor": map[string]any{
			"drop_on_queue_full":    true,
			"max_queue_size":        1024,
			"batch_timeout":         2.5,
			"inactive_timeout":      1.0,
			"max_export_batch_size": 16,
		},
		"set_ngx_var": true,
	}
	if err := util.Validate(metadata, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected supported document: %v", err)
	}

	for _, tt := range []struct {
		name     string
		metadata map[string]any
	}{
		{
			name:     "invalid trace id source",
			metadata: map[string]any{"trace_id_source": "request-id"},
		},
		{
			name: "resource nested value",
			metadata: map[string]any{
				"resource": map[string]any{"nested": map[string]any{"value": "invalid"}},
			},
		},
		{
			name: "request header array value",
			metadata: map[string]any{
				"collector": map[string]any{
					"request_headers": map[string]any{"X-Values": []any{"invalid"}},
				},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := util.Validate(tt.metadata, p.GetMetadataSchema()); err == nil {
				t.Fatal("metadata schema error = nil, want rejection")
			}
		})
	}
}

func TestLoadMetadataRetainsRuntimeJSONNumbersInResourceAttributes(t *testing.T) {
	view := mustOpenTelemetryMetadataView(t, map[string]string{
		name: `{"resource":{"positive_int":7,"negative_int":-3,"fraction":1.25,"negative_fraction":-0.5}}`,
	})
	metadata, configured, err := loadMetadata(view, nil)
	if err != nil {
		t.Fatalf("loadMetadata() error = %v", err)
	}
	if !configured {
		t.Fatal("metadata configured = false, want true")
	}

	values := make(map[string]attribute.Value)
	for _, item := range otelResource(metadata.Resource).Attributes() {
		values[string(item.Key)] = item.Value
	}
	for key, want := range map[string]struct {
		typeName attribute.Type
		intValue int64
		float    float64
	}{
		"positive_int":      {typeName: attribute.INT64, intValue: 7},
		"negative_int":      {typeName: attribute.INT64, intValue: -3},
		"fraction":          {typeName: attribute.FLOAT64, float: 1.25},
		"negative_fraction": {typeName: attribute.FLOAT64, float: -0.5},
	} {
		value, ok := values[key]
		if !ok {
			t.Fatalf("resource attribute %q missing; got %#v", key, values)
		}
		if value.Type() != want.typeName {
			t.Fatalf("resource attribute %q type = %v, want %v", key, value.Type(), want.typeName)
		}
		if want.typeName == attribute.INT64 && value.AsInt64() != want.intValue {
			t.Fatalf("resource attribute %q = %d, want %d", key, value.AsInt64(), want.intValue)
		}
		if want.typeName == attribute.FLOAT64 && value.AsFloat64() != want.float {
			t.Fatalf("resource attribute %q = %v, want %v", key, value.AsFloat64(), want.float)
		}
	}

	standardNumberValues := make(map[string]attribute.Value)
	for _, item := range otelResource(map[string]any{
		"standard_int":      stdjson.Number("11"),
		"standard_fraction": stdjson.Number("-2.75"),
	}).Attributes() {
		standardNumberValues[string(item.Key)] = item.Value
	}
	if got := standardNumberValues["standard_int"]; got.Type() != attribute.INT64 || got.AsInt64() != 11 {
		t.Fatalf("standard encoding/json integer = %#v, want int64 11", got)
	}
	if got := standardNumberValues["standard_fraction"]; got.Type() != attribute.FLOAT64 || got.AsFloat64() != -2.75 {
		t.Fatalf("standard encoding/json fraction = %#v, want float64 -2.75", got)
	}
}

func TestLoadMetadataPrecedence(t *testing.T) {
	attr := func(traceSource, address string) map[string]any {
		return map[string]any{
			"trace_id_source": traceSource,
			"resource":        map[string]any{"service.name": traceSource},
			"collector": map[string]any{
				"address":         address,
				"request_timeout": 7,
				"request_headers": map[string]any{"Authorization": traceSource},
			},
		}
	}
	metadataDocument := func(traceSource, address string) string {
		return fmt.Sprintf(
			`{"trace_id_source":%q,"resource":{"service.name":%q},"collector":{"address":%q,"request_timeout":9,"request_headers":{"Authorization":%q}}}`,
			traceSource,
			traceSource,
			address,
			traceSource,
		)
	}
	tests := []struct {
		name           string
		view           map[string]string
		pluginAttr     map[string]map[string]any
		wantSource     string
		wantAddress    string
		wantTimeout    int
		wantConfigured bool
		wantResource   string
		wantHeader     string
	}{
		{
			name:           "defaults",
			wantSource:     "random",
			wantAddress:    "127.0.0.1:4318",
			wantTimeout:    3,
			wantConfigured: false,
		},
		{
			name:           "canonical attr",
			pluginAttr:     map[string]map[string]any{name: attr("attr-canonical", "attr-canonical:4318")},
			wantSource:     "attr-canonical",
			wantAddress:    "attr-canonical:4318",
			wantTimeout:    7,
			wantConfigured: true,
			wantResource:   "attr-canonical",
			wantHeader:     "attr-canonical",
		},
		{
			name:           "canonical metadata wins over canonical attr",
			view:           map[string]string{name: metadataDocument("metadata-canonical", "metadata-canonical:4318")},
			pluginAttr:     map[string]map[string]any{name: attr("attr-canonical", "attr-canonical:4318")},
			wantSource:     "metadata-canonical",
			wantAddress:    "metadata-canonical:4318",
			wantTimeout:    9,
			wantConfigured: true,
			wantResource:   "metadata-canonical",
			wantHeader:     "metadata-canonical",
		},
		{
			name: "metadata replaces rather than merges attrs",
			view: map[string]string{
				name: `{"trace_id_source":"metadata-only"}`,
			},
			pluginAttr:     map[string]map[string]any{name: attr("attr-canonical", "attr-canonical:4318")},
			wantSource:     "metadata-only",
			wantAddress:    "127.0.0.1:4318",
			wantTimeout:    3,
			wantConfigured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := mustOpenTelemetryMetadataView(t, tt.view)
			metadata, configured, err := loadMetadata(view, tt.pluginAttr)
			if err != nil {
				t.Fatalf("loadMetadata() error = %v", err)
			}
			if configured != tt.wantConfigured {
				t.Fatalf("configured = %v, want %v", configured, tt.wantConfigured)
			}
			if metadata.TraceIDSource != tt.wantSource || metadata.Collector.Address != tt.wantAddress ||
				metadata.Collector.RequestTimeout != tt.wantTimeout {
				t.Fatalf(
					"metadata source/address/timeout = %q/%q/%d, want %q/%q/%d",
					metadata.TraceIDSource,
					metadata.Collector.Address,
					metadata.Collector.RequestTimeout,
					tt.wantSource,
					tt.wantAddress,
					tt.wantTimeout,
				)
			}
			if tt.wantResource != "" && metadata.Resource["service.name"] != tt.wantResource {
				t.Fatalf("resource service.name = %q, want %q", metadata.Resource["service.name"], tt.wantResource)
			}
			if tt.wantHeader != "" && metadata.Collector.RequestHeaders["Authorization"] != tt.wantHeader {
				t.Fatalf(
					"authorization header = %v, want %q",
					metadata.Collector.RequestHeaders["Authorization"],
					tt.wantHeader,
				)
			}
		})
	}

	canonicalInvalid := mustOpenTelemetryMetadataView(
		t,
		map[string]string{name: `{"collector":{"request_timeout":"invalid"}}`},
	)
	if _, _, err := loadMetadata(
		canonicalInvalid,
		map[string]map[string]any{name: attr("fallback", "fallback:4318")},
	); err == nil {
		t.Fatal("loadMetadata() error = nil for invalid canonical metadata")
	}

	for _, tt := range []struct {
		name       string
		pluginAttr map[string]map[string]any
	}{
		{
			name:       "canonical nil blocks defaults",
			pluginAttr: map[string]map[string]any{name: nil},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata, configured, err := loadMetadata(runtime.MetadataView{}, tt.pluginAttr)
			if err == nil {
				t.Fatal("loadMetadata() error = nil for nil plugin attribute")
			}
			if configured || metadata.TraceIDSource != "" || metadata.Resource != nil ||
				metadata.Collector.Address != "" || metadata.Collector.RequestTimeout != 0 ||
				metadata.Collector.RequestHeaders != nil || metadata.SetNgxVar ||
				metadata.BatchSpanProcessor != (BatchSpanProcessorConfig{}) {
				t.Fatalf("loadMetadata() = (%#v, %v, %v), want fail-closed zero metadata", metadata, configured, err)
			}
		})
	}
}

func TestPreparedGenerationsRetainOpenTelemetryMetadata(t *testing.T) {
	nSource := []byte(
		`{"trace_id_source":"x-request-id","resource":{"service.name":"generation-n"},"collector":{"address":"127.0.0.1:4318","request_headers":{"Authorization":"generation-n"}}}`,
	)
	nView, err := runtime.NewMetadataView(map[string][]byte{name: nSource})
	if err != nil {
		t.Fatalf("NewMetadataView(N) error = %v", err)
	}
	nSource[0] = 'x'

	pN := &Plugin{}
	pN.SetDependencies(base.Dependencies{
		Config: &config.EffectiveConfig{}, Metadata: nView, Tasks: newOpenTelemetryTaskOwner(t),
	})
	if err := pN.Init(); err != nil {
		t.Fatalf("N Init() error = %v", err)
	}
	if err := pN.PostInit(); err != nil {
		t.Fatalf("N PostInit() error = %v", err)
	}
	t.Cleanup(pN.Stop)
	nProvider := pN.tracerProvider

	nPlusOneView := mustOpenTelemetryMetadataView(t, map[string]string{
		name: `{"trace_id_source":"random","resource":{"service.name":"generation-n-plus-one"},"collector":{"address":"127.0.0.1:4319","request_headers":{"Authorization":"generation-n-plus-one"}}}`,
	})
	pNPlusOne := &Plugin{}
	pNPlusOne.SetDependencies(base.Dependencies{
		Config: &config.EffectiveConfig{}, Metadata: nPlusOneView, Tasks: newOpenTelemetryTaskOwner(t),
	})
	if err := pNPlusOne.Init(); err != nil {
		t.Fatalf("N+1 Init() error = %v", err)
	}
	if err := pNPlusOne.PostInit(); err != nil {
		t.Fatalf("N+1 PostInit() error = %v", err)
	}
	t.Cleanup(pNPlusOne.Stop)

	if pN.metadata.TraceIDSource != "x-request-id" || pN.metadata.Resource["service.name"] != "generation-n" ||
		pN.metadata.Collector.RequestHeaders["Authorization"] != "generation-n" {
		t.Fatalf("N metadata changed after N+1 construction: %#v", pN.metadata)
	}
	if pN.tracerProvider != nProvider || pNPlusOne.tracerProvider == nProvider {
		t.Fatal("generations did not retain independent tracer providers")
	}
	if pNPlusOne.metadata.TraceIDSource != "random" ||
		pNPlusOne.metadata.Resource["service.name"] != "generation-n-plus-one" ||
		pNPlusOne.metadata.Collector.Address != "127.0.0.1:4319" {
		t.Fatalf("N+1 metadata = %#v, want N+1 values", pNPlusOne.metadata)
	}
}

type countingSpanExporter struct {
	shutdown atomic.Int32
	exports  atomic.Int32
}

func (e *countingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	e.exports.Add(1)
	return nil
}

func (e *countingSpanExporter) Shutdown(context.Context) error {
	e.shutdown.Add(1)
	return nil
}

func TestStopShutsDownOnlyItsOpenTelemetryProvider(t *testing.T) {
	exporterN := &countingSpanExporter{}
	providerN := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporterN, sdktrace.WithBatchTimeout(time.Millisecond)),
	)
	exporterNPlusOne := &countingSpanExporter{}
	providerNPlusOne := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporterNPlusOne, sdktrace.WithBatchTimeout(time.Millisecond)),
	)
	pN := &Plugin{tracerProvider: providerN}
	pNPlusOne := &Plugin{tracerProvider: providerNPlusOne}

	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			pN.Stop()
		})
	}
	wait.Wait()
	if got := exporterN.shutdown.Load(); got != 1 {
		t.Fatalf("N exporter shutdown count = %d, want 1", got)
	}

	tracer := pNPlusOne.tracer()
	_, span := tracer.Start(context.Background(), "generation-n-plus-one")
	span.End()
	if err := providerNPlusOne.ForceFlush(context.Background()); err != nil {
		t.Fatalf("N+1 ForceFlush() error = %v", err)
	}
	if got := exporterNPlusOne.shutdown.Load(); got != 0 {
		t.Fatalf("N+1 exporter shutdown count after N Stop() = %d, want 0", got)
	}
	pNPlusOne.Stop()
	if got := exporterNPlusOne.shutdown.Load(); got != 1 {
		t.Fatalf("N+1 exporter shutdown count = %d, want 1", got)
	}
}

func mustOpenTelemetryMetadataView(t *testing.T, documents map[string]string) runtime.MetadataView {
	t.Helper()
	encoded := make(map[string][]byte, len(documents))
	for factory, document := range documents {
		encoded[factory] = []byte(document)
	}
	view, err := runtime.NewMetadataView(encoded)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestNewTracerProviderAcceptsSetNgxVar(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	metadata := Metadata{
		SetNgxVar: true,
		Collector: CollectorConfig{Address: collector.URL},
	}

	provider, _, err := newTracerProviderWithProcessor(
		SamplerConfig{Name: "always_on"}, metadata, true, newOpenTelemetryTaskOwner(t),
	)
	if err != nil {
		t.Fatalf("newTracerProviderWithProcessor() rejected APISIX 3.17 set_ngx_var: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
}

func TestNewTracerProviderAcceptsAPISIX317PositiveInactiveTimeout(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)
	metadata := Metadata{
		Collector: CollectorConfig{Address: collector.URL},
		BatchSpanProcessor: BatchSpanProcessorConfig{
			InactiveTimeout: new(0.5),
		},
	}

	provider, _, err := newTracerProviderWithProcessor(
		SamplerConfig{Name: "always_on"}, metadata, true, newOpenTelemetryTaskOwner(t),
	)
	if err != nil {
		t.Fatalf("newTracerProviderWithProcessor() rejected APISIX 3.17 inactive_timeout=0.5: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
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
	for key, want := range map[string]string{
		"telemetry.sdk.language": "lua",
		"telemetry.sdk.name":     "opentelemetry-lua",
		"telemetry.sdk.version":  "0.1.1",
	} {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q; attrs=%#v", key, got[key], want, got)
		}
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
			MaxQueueSize:       new(8),
			BatchTimeout:       new(0.01),
			MaxExportBatchSize: new(1),
		},
	}
	provider, _, err := newTracerProviderWithProcessor(
		SamplerConfig{Name: "always_on"}, metadata, true, newOpenTelemetryTaskOwner(t),
	)
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
	effective := &config.EffectiveConfig{Config: config.Config{
		PluginAttr: map[string]map[string]any{
			name: {
				"collector": map[string]any{"address": "://invalid"},
			},
		},
	}}

	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: effective})
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

func TestPostInitRejectsNegativeInactiveTimeout(t *testing.T) {
	effective := &config.EffectiveConfig{Config: config.Config{
		PluginAttr: map[string]map[string]any{name: {
			"batch_span_processor": map[string]any{"inactive_timeout": -1.0},
		}},
	}}

	p := &Plugin{}
	p.SetDependencies(base.Dependencies{Config: effective})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil {
		t.Fatal("PostInit() error = nil, want invalid inactive_timeout rejection")
	}
	const want = "opentelemetry inactive_timeout must be greater than 0"
	if err.Error() != want {
		t.Fatalf("PostInit() error = %q, want %q", err, want)
	}
	if p.tracerProvider == nil {
		t.Fatal("PostInit() fallback tracer provider = nil")
	}
}

func newOpenTelemetryTaskOwner(t *testing.T) *runtime.TaskOwner {
	t.Helper()
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/opentelemetry/test", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	t.Cleanup(func() {
		if residuals, stopErr := tasks.Stop(context.Background()); stopErr != nil || len(residuals) != 0 {
			t.Errorf("TaskRegistry.Stop() = (%v, %v)", residuals, stopErr)
		}
	})
	return owner
}

func TestTraceRequestPhaseSetsAPISIX317VariablesWhenEnabled(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	for _, enabled := range []bool{true, false} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			p := &Plugin{
				metadata:       Metadata{SetNgxVar: enabled},
				tracerProvider: provider,
			}
			request, lifecycle := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil),
				time.Now(),
			)
			result := p.RunRequestPhase(httptest.NewRecorder(), request)
			spanContext := trace.SpanFromContext(result.Request.Context()).SpanContext()
			variables := map[string]any{
				"$opentelemetry_context_traceparent": apisixctx.GetRequestVar(
					result.Request,
					"$opentelemetry_context_traceparent",
				),
				"$opentelemetry_trace_id": apisixctx.GetRequestVar(result.Request, "$opentelemetry_trace_id"),
				"$opentelemetry_span_id":  apisixctx.GetRequestVar(result.Request, "$opentelemetry_span_id"),
			}
			if enabled {
				wantTraceparent := fmt.Sprintf(
					"00-%s-%s-%02x",
					spanContext.TraceID(),
					spanContext.SpanID(),
					byte(spanContext.TraceFlags()),
				)
				if variables["$opentelemetry_context_traceparent"] != wantTraceparent ||
					variables["$opentelemetry_trace_id"] != spanContext.TraceID().String() ||
					variables["$opentelemetry_span_id"] != spanContext.SpanID().String() {
					t.Fatalf("OpenTelemetry variables = %#v, want traceparent=%q trace=%s span=%s", variables,
						wantTraceparent, spanContext.TraceID(), spanContext.SpanID())
				}
			} else {
				for name, value := range variables {
					if value != nil {
						t.Fatalf("%s = %#v, want unset", name, value)
					}
				}
			}
			lifecycle.Complete(
				apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
				time.Now(),
			)
			if failures := lifecycle.Finalize(); len(failures) != 0 {
				t.Fatalf("lifecycle failures = %#v", failures)
			}
		})
	}
}

func TestTraceStartsAtInheritedRewriteAndEndsOnce(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{config: Config{Sampler: SamplerConfig{Name: "always_on"}}, tracerProvider: provider}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("request phase decision = %d, want continue", result.Decision)
	}
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusCreated, Bytes: 3, Committed: true,
	}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("second lifecycle finalization failures = %#v", failures)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one dynamic export", len(spans))
	}
}

func TestTraceRequestPhaseExtractsRemoteParentAndSkipsHealthCheck(t *testing.T) {
	previous := otelapi.GetTextMapPropagator()
	otelapi.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otelapi.SetTextMapPropagator(previous) })

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	request.Header.Set(
		"traceparent",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one", len(spans))
	}
	if got := spans[0].Parent().SpanID().String(); got != "00f067aa0ba902b7" {
		t.Fatalf("parent span ID = %q, want remote parent", got)
	}
	if !spans[0].Parent().IsRemote() {
		t.Fatal("parent span is not marked remote")
	}
	if result.Request == nil {
		t.Fatal("RunRequestPhase() returned nil request")
	}
	propagated := result.Request.Header.Get("traceparent")
	parts := strings.Split(propagated, "-")
	if len(parts) != 4 || parts[1] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("propagated traceparent = %q, want inherited trace ID", propagated)
	}
	if parts[2] == "00f067aa0ba902b7" {
		t.Fatalf("propagated traceparent = %q, want current server span ID", propagated)
	}

	healthRequest, healthLifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/healthz", nil), time.Now(),
	)
	healthResult := p.RunRequestPhase(httptest.NewRecorder(), healthRequest)
	healthLifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Now(),
	)
	if failures := healthLifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("health lifecycle failures = %#v", failures)
	}
	if got := len(recorder.Ended()); got != 1 {
		t.Fatalf("ended spans after health check = %d, want unchanged", got)
	}
	if healthResult.Request == nil {
		t.Fatal("health request phase returned nil request")
	}
}

func TestTraceUsesRoutePatternNameAndServerErrorStatus(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{
		tracerProvider: provider,
		route:          resource.Route{Uri: "/orders/:id"},
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders/123", nil), time.Now(),
	)
	p.RunRequestPhase(httptest.NewRecorder(), request)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{
			Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusInternalServerError, Committed: true,
		},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one", len(spans))
	}
	if got := spans[0].Name(); got != "GET /orders/:id" {
		t.Fatalf("span name = %q, want route pattern", got)
	}
	if got := spans[0].Status().Code; got != codes.Error {
		t.Fatalf("span status = %v, want Error", got)
	}
}

func TestTraceUsesActuallyMatchedRoutePatternForMultipleURIs(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{
		tracerProvider: provider,
		route:          resource.Route{Uris: []string{"/orders/:id", "/purchases/:id"}},
	}
	router := chi.NewRouter()
	router.Get("/purchases/{id}", func(_ http.ResponseWriter, request *http.Request) {
		request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
		p.RunRequestPhase(httptest.NewRecorder(), request)
		lifecycle.Complete(
			apisixctx.ResponseOutcome{
				Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent, Committed: true,
			},
			time.Now(),
		)
		if failures := lifecycle.Finalize(); len(failures) != 0 {
			t.Fatalf("lifecycle failures = %#v", failures)
		}
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/purchases/42", nil))

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one", len(spans))
	}
	if got := spans[0].Name(); got != "GET /purchases/{id}" {
		t.Fatalf("span name = %q, want actually matched pattern", got)
	}
	attributes := map[string]string{}
	for _, item := range spans[0].Attributes() {
		attributes[string(item.Key)] = item.Value.AsString()
	}
	if attributes["http.route"] != "/purchases/{id}" {
		t.Fatalf("http.route = %q, want actually matched pattern", attributes["http.route"])
	}
}

func TestUnsampledTraceRegistersNoExportFinalizer(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.NeverSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
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
	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("ended spans = %d, want no exporter finalizer for unsampled start", got)
	}
}

func TestTraceUsesFinalReplacementRequestSourceAndOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/initial", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	replacement := result.Request.Clone(result.Request.Context())
	replacement.URL.Path = "/replacement"
	replacement.Header.Set("X-Request-Id", "trace-request-1")
	apisixctx.RegisterRequestVar(replacement, "$retry_count", 2)
	apisixctx.RegisterRequestVar(replacement, "$upstream_status", http.StatusNotModified)
	lifecycle.SetFinalRequest(replacement)
	apisixctx.SetResponseSource(replacement, apisixctx.ResponseSourceCacheHit)
	finished := time.Now()
	lifecycle.Complete(apisixctx.ResponseOutcome{
		Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNotModified, Bytes: 9, Committed: true,
	}, finished)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one", len(spans))
	}
	attrs := map[string]string{}
	for _, attr := range spans[0].Attributes() {
		attrs[string(attr.Key)] = attr.Value.String()
	}
	if attrs["http.status_code"] != "304" {
		t.Fatalf("http.status_code = %q, want 304", attrs["http.status_code"])
	}
	if attrs["apisix.response_source"] != string(apisixctx.ResponseSourceCacheHit) {
		t.Fatalf("response source = %q, want cache_hit", attrs["apisix.response_source"])
	}
	if attrs["http.response.status_code"] != "304" {
		t.Fatalf("http.response.status_code = %q, want 304", attrs["http.response.status_code"])
	}
	for _, key := range []string{
		"http.response_content_length", "apisix.request_id", "apisix.node_id",
		"apisix.outcome", "apisix.retry_count", "http.upstream_status_code",
	} {
		if _, exists := attrs[key]; exists {
			t.Fatalf("non-APISIX OpenTelemetry attribute %q = %q; attrs=%#v", key, attrs[key], attrs)
		}
	}
}

func TestTraceFinalizerCapturesHeadersAddedByLaterPlugin(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{
		config: Config{
			AdditionalHeaderPrefixAttributes: []string{"x-injected-*"},
		},
		tracerProvider: provider,
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	laterRequest := result.Request.Clone(result.Request.Context())
	laterRequest.Header.Set("X-Injected-By-Plugin", "test-value")
	lifecycle.SetFinalRequest(laterRequest)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle failures = %#v", failures)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want one", len(spans))
	}
	for _, item := range spans[0].Attributes() {
		if item.Key == "x-injected-by-plugin" {
			if got := item.Value.AsString(); got != "test-value" {
				t.Fatalf("x-injected-by-plugin = %q, want test-value", got)
			}
			return
		}
	}
	t.Fatal("final span does not contain the request header added by the later plugin")
}

func TestTracerDirectHandlerDoesNotDuplicateProductionOwner(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
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
	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("ended spans = %d, want no direct-handler duplicate", got)
	}
}
