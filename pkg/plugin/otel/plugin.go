package otel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/riandyrn/otelchi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// version  = "0.1"
	priority = 12009
	name     = "opentelemetry"
)

const schema = `
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "sampler": {
      "type": "object",
      "properties": {
        "name": {
          "type": "string",
          "enum": ["always_on", "always_off", "trace_id_ratio", "parent_base"],
          "default": "always_off"
        },
        "options": {
          "type": "object",
          "properties": {
            "fraction": {
              "type": "number",
              "default": 0
            },
            "root": {
              "type": "object",
              "properties": {
                "name": {
                  "type": "string",
                  "enum": ["always_on", "always_off", "trace_id_ratio"],
                  "default": "always_off"
                },
                "options": {
                  "type": "object",
                  "properties": {
                    "fraction": {
                      "type": "number",
                      "default": 0
                    }
                  }
                }
              }
            }
          }
        }
      }
    },
    "additional_attributes": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "additional_header_prefix_attributes": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "server_name": {
      "type": "string"
    }
  }
}
`

type Plugin struct {
	base.BasePlugin
	config Config

	metadata       Metadata
	tracerProvider *sdktrace.TracerProvider
	route          resource.Route
	service        resource.Service
}

type Metadata struct {
	TraceIDSource      string                   `json:"trace_id_source,omitempty"`
	Resource           map[string]any           `json:"resource,omitempty"`
	Collector          CollectorConfig          `json:"collector,omitempty"`
	BatchSpanProcessor BatchSpanProcessorConfig `json:"batch_span_processor,omitempty"`
	SetNgxVar          bool                     `json:"set_ngx_var,omitempty"`
}

type CollectorConfig struct {
	Address        string         `json:"address,omitempty"`
	RequestTimeout int            `json:"request_timeout,omitempty"`
	RequestHeaders map[string]any `json:"request_headers,omitempty"`
}

type BatchSpanProcessorConfig struct {
	DropOnQueueFull    bool    `json:"drop_on_queue_full,omitempty"`
	MaxQueueSize       int     `json:"max_queue_size,omitempty"`
	BatchTimeout       float64 `json:"batch_timeout,omitempty"`
	InactiveTimeout    float64 `json:"inactive_timeout,omitempty"`
	MaxExportBatchSize int     `json:"max_export_batch_size,omitempty"`
}

type Config struct {
	Sampler                          SamplerConfig `json:"sampler,omitempty"`
	AdditionalAttributes             []string      `json:"additional_attributes,omitempty"`
	AdditionalHeaderPrefixAttributes []string      `json:"additional_header_prefix_attributes,omitempty"`
	ServerName                       string        `json:"server_name,omitempty"`
}

type SamplerConfig struct {
	Name    string         `json:"name,omitempty"`
	Options SamplerOptions `json:"options,omitempty"`
}

type SamplerOptions struct {
	Fraction float64           `json:"fraction,omitempty"`
	Root     RootSamplerConfig `json:"root,omitempty"`
}

type RootSamplerConfig struct {
	Name    string             `json:"name,omitempty"`
	Options RootSamplerOptions `json:"options,omitempty"`
}

type RootSamplerOptions struct {
	Fraction float64 `json:"fraction,omitempty"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Sampler.Name == "" {
		p.config.Sampler.Name = "always_off"
	}
	if p.config.Sampler.Options.Root.Name == "" {
		p.config.Sampler.Options.Root.Name = "always_off"
	}
	metadata, configured := loadMetadata()
	p.metadata = metadata

	var err error
	p.tracerProvider, err = newTracerProvider(p.config.Sampler, metadata, configured)
	if err != nil {
		p.tracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSampler(buildSampler(p.config.Sampler)))
	}
	return err
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	wrappedNext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attrs := append(p.resourceSpanAttributes(), p.additionalSpanAttributes(r)...)
		if len(attrs) > 0 {
			trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
		}
		next.ServeHTTP(w, r)
	})
	opts := []otelchi.Option{
		otelchi.WithFilter(func(r *http.Request) bool {
			if r.URL.Path == "/healthz" {
				return false
			}
			return true
		}),
		otelchi.WithRequestMethodInSpanName(true),
		otelchi.WithTracerProvider(p.tracerProvider),
	}

	handler := otelchi.Middleware(p.serverName(), opts...)(wrappedNext)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.metadata.TraceIDSource == "x-request-id" {
			requestID := r.Header.Get("X-Request-ID")
			r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
		}
		handler.ServeHTTP(w, r)
	})
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.route = route
	p.service = service
}

func (p *Plugin) Stop() {
	if p.tracerProvider == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.tracerProvider.Shutdown(ctx)
}

func (p *Plugin) serverName() string {
	if p.config.ServerName != "" {
		return p.config.ServerName
	}
	return "APISIX"
}

func buildSampler(conf SamplerConfig) sdktrace.Sampler {
	switch conf.Name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "trace_id_ratio":
		return sdktrace.TraceIDRatioBased(conf.Options.Fraction)
	case "parent_base":
		return sdktrace.ParentBased(buildRootSampler(conf.Options.Root))
	default:
		return sdktrace.NeverSample()
	}
}

func buildRootSampler(conf RootSamplerConfig) sdktrace.Sampler {
	switch conf.Name {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "trace_id_ratio":
		return sdktrace.TraceIDRatioBased(conf.Options.Fraction)
	default:
		return sdktrace.NeverSample()
	}
}

func loadMetadata() (metadata Metadata, configured bool) {
	if config.GlobalConfig != nil {
		if attr := config.GlobalConfig.PluginAttr[name]; attr != nil {
			if err := util.Parse(attr, &metadata); err == nil {
				configured = true
			}
		}
	}

	var stored Metadata
	if safeGetPluginMetadata(name, &stored) == nil {
		metadata = stored
		configured = true
	}
	if !configured {
		return Metadata{}, false
	}
	if metadata.TraceIDSource == "" {
		metadata.TraceIDSource = "random"
	}
	if metadata.Collector.Address == "" {
		metadata.Collector.Address = "127.0.0.1:4318"
	}
	if metadata.Collector.RequestTimeout == 0 {
		metadata.Collector.RequestTimeout = 3
	}
	return metadata, true
}

func safeGetPluginMetadata(id string, target any) (err error) {
	defer func() {
		if recover() != nil {
			err = store.ErrNotFound
		}
	}()
	return store.GetPluginMetadata(id, target)
}

func newTracerProvider(
	sampler SamplerConfig,
	metadata Metadata,
	metadataConfigured bool,
) (*sdktrace.TracerProvider, error) {
	options := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(buildSampler(sampler)),
		sdktrace.WithResource(otelResource(metadata.Resource)),
	}
	if metadata.TraceIDSource == "x-request-id" {
		options = append(options, sdktrace.WithIDGenerator(requestIDGenerator{}))
	}
	if !metadataConfigured {
		return sdktrace.NewTracerProvider(options...), nil
	}

	exporterOptions := []otlptracehttp.Option{
		otlptracehttp.WithTimeout(time.Duration(metadata.Collector.RequestTimeout) * time.Second),
		otlptracehttp.WithHeaders(stringHeaders(metadata.Collector.RequestHeaders)),
	}
	address := metadata.Collector.Address
	if strings.Contains(address, "://") {
		collectorURL, err := url.Parse(address)
		if err != nil || collectorURL.Host == "" ||
			(collectorURL.Scheme != "http" && collectorURL.Scheme != "https") {
			return nil, fmt.Errorf("invalid OpenTelemetry collector address %q", address)
		}
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(address))
	} else {
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpoint(address), otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(context.Background(), exporterOptions...)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry OTLP exporter: %w", err)
	}

	batchOptions := batchSpanProcessorOptions(metadata.BatchSpanProcessor)
	options = append(options, sdktrace.WithBatcher(exporter, batchOptions...))
	return sdktrace.NewTracerProvider(options...), nil
}

func batchSpanProcessorOptions(config BatchSpanProcessorConfig) []sdktrace.BatchSpanProcessorOption {
	options := make([]sdktrace.BatchSpanProcessorOption, 0, 5)
	if !config.DropOnQueueFull {
		options = append(options, sdktrace.WithBlocking())
	}
	if config.MaxQueueSize > 0 {
		options = append(options, sdktrace.WithMaxQueueSize(config.MaxQueueSize))
	}
	if config.BatchTimeout > 0 {
		options = append(options, sdktrace.WithBatchTimeout(time.Duration(config.BatchTimeout*float64(time.Second))))
	}
	if config.InactiveTimeout > 0 {
		options = append(
			options,
			sdktrace.WithExportTimeout(time.Duration(config.InactiveTimeout*float64(time.Second))),
		)
	}
	if config.MaxExportBatchSize > 0 {
		options = append(options, sdktrace.WithMaxExportBatchSize(config.MaxExportBatchSize))
	}
	return options
}

func otelResource(configured map[string]any) *sdkresource.Resource {
	hostname, _ := os.Hostname()
	attributes := []attribute.KeyValue{attribute.String("hostname", hostname)}
	if _, ok := configured["service.name"]; !ok {
		attributes = append(attributes, attribute.String("service.name", "APISIX"))
	}
	for key, value := range configured {
		switch typed := value.(type) {
		case string:
			attributes = append(attributes, attribute.String(key, typed))
		case bool:
			attributes = append(attributes, attribute.Bool(key, typed))
		case float64:
			attributes = append(attributes, attribute.Float64(key, typed))
		case int:
			attributes = append(attributes, attribute.Int(key, typed))
		}
	}
	return sdkresource.NewWithAttributes("", attributes...)
}

func stringHeaders(headers map[string]any) map[string]string {
	result := make(map[string]string, len(headers))
	for key, value := range headers {
		result[key] = fmt.Sprint(value)
	}
	return result
}

func (p *Plugin) additionalSpanAttributes(r *http.Request) []attribute.KeyValue {
	attrs := make(
		[]attribute.KeyValue,
		0,
		len(p.config.AdditionalAttributes)+len(p.config.AdditionalHeaderPrefixAttributes),
	)
	for _, name := range p.config.AdditionalAttributes {
		if value, ok := p.requestVariable(r, name); ok {
			attrs = append(attrs, attribute.String(name, value))
		}
	}

	headers := normalizedHeaders(r.Header)
	for _, key := range p.config.AdditionalHeaderPrefixAttributes {
		key = strings.ToLower(key)
		if strings.HasSuffix(key, "*") && len(key) > 1 {
			prefix := strings.TrimSuffix(key, "*")
			for header, value := range headers {
				if strings.HasPrefix(header, prefix) && value != "" {
					attrs = append(attrs, attribute.String(header, value))
				}
			}
			continue
		}

		if value := headers[key]; value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	return attrs
}

func (p *Plugin) requestVariable(r *http.Request, name string) (string, bool) {
	key := "$" + strings.TrimPrefix(name, "$")
	if value := v.GetNginxVar(r, key); value != "" {
		return value, true
	}
	if value, ok := coerceAttributeValue(apisixctx.GetApisixVar(r, key)); ok {
		return value, true
	}
	if value, ok := coerceAttributeValue(apisixctx.GetRequestVar(r, key)); ok {
		return value, true
	}
	switch key {
	case "$route_id":
		return nonEmptyValue(p.route.ID)
	case "$route_name":
		return nonEmptyValue(p.route.Name)
	case "$matched_uri":
		return nonEmptyValue(matchedRouteURI(p.route))
	case "$service_id":
		return nonEmptyValue(p.route.ServiceID)
	case "$service_name":
		return nonEmptyValue(p.service.Name)
	}
	return "", false
}

func (p *Plugin) resourceSpanAttributes() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	if p.route.ID != "" {
		attrs = append(attrs, attribute.String("apisix.route_id", p.route.ID))
	}
	if p.route.Name != "" {
		attrs = append(attrs, attribute.String("apisix.route_name", p.route.Name))
	}
	if routeURI := matchedRouteURI(p.route); routeURI != "" {
		attrs = append(attrs, attribute.String("http.route", routeURI))
	}
	if p.route.ServiceID != "" {
		attrs = append(attrs, attribute.String("apisix.service_id", p.route.ServiceID))
	}
	if p.service.Name != "" {
		attrs = append(attrs, attribute.String("apisix.service_name", p.service.Name))
	}
	return attrs
}

func matchedRouteURI(route resource.Route) string {
	if route.Uri != "" {
		return route.Uri
	}
	if len(route.Uris) > 0 {
		return route.Uris[0]
	}
	return ""
}

func nonEmptyValue(value string) (string, bool) {
	return value, value != ""
}

func coerceAttributeValue(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	text := fmt.Sprint(value)
	if text == "" {
		return "", false
	}
	return text, true
}

func normalizedHeaders(headers http.Header) map[string]string {
	values := make(map[string]string, len(headers))
	for key, headerValues := range headers {
		if len(headerValues) == 0 {
			continue
		}
		values[strings.ToLower(key)] = strings.Join(headerValues, ", ")
	}
	return values
}

type requestIDContextKey struct{}

type requestIDGenerator struct{}

func (requestIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	traceID := traceIDFromRequestID(ctx.Value(requestIDContextKey{}))
	if !traceID.IsValid() {
		traceID = randomTraceID()
	}
	return traceID, randomSpanID()
}

func (requestIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return randomSpanID()
}

func traceIDFromRequestID(value any) trace.TraceID {
	requestID, _ := value.(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return trace.TraceID{}
	}
	if decoded, err := hex.DecodeString(requestID); err == nil && len(decoded) == len(trace.TraceID{}) {
		var traceID trace.TraceID
		copy(traceID[:], decoded)
		if traceID.IsValid() {
			return traceID
		}
	}

	sum := sha256.Sum256([]byte(requestID))
	var traceID trace.TraceID
	copy(traceID[:], sum[:len(traceID)])
	return traceID
}

func randomTraceID() trace.TraceID {
	var traceID trace.TraceID
	for !traceID.IsValid() {
		if _, err := rand.Read(traceID[:]); err != nil {
			panic(fmt.Sprintf("generate OpenTelemetry trace ID: %s", err))
		}
	}
	return traceID
}

func randomSpanID() trace.SpanID {
	var spanID trace.SpanID
	for !spanID.IsValid() {
		if _, err := rand.Read(spanID[:]); err != nil {
			panic(fmt.Sprintf("generate OpenTelemetry span ID: %s", err))
		}
	}
	return spanID
}
