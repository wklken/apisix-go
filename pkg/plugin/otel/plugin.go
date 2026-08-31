package otel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/riandyrn/otelchi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
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

const metadataSchema = `
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "trace_id_source": {
      "type": "string",
      "enum": ["x-request-id", "random"]
    },
    "resource": {
      "type": "object",
      "additionalProperties": {
        "type": ["boolean", "number", "string"]
      }
    },
    "collector": {
      "type": "object",
      "properties": {
        "address": {
          "type": "string"
        },
        "request_timeout": {
          "type": "integer"
        },
        "request_headers": {
          "type": "object",
          "additionalProperties": {
            "type": ["boolean", "number", "string"]
          }
        }
      },
      "additionalProperties": true
    },
    "batch_span_processor": {
      "type": "object",
      "properties": {
        "drop_on_queue_full": {
          "type": "boolean"
        },
        "max_queue_size": {
          "type": "integer"
        },
        "batch_timeout": {
          "type": "number"
        },
        "inactive_timeout": {
          "type": "number"
        },
        "max_export_batch_size": {
          "type": "integer"
        }
      },
      "additionalProperties": true
    },
    "set_ngx_var": {
      "type": "boolean"
    }
  },
  "additionalProperties": true
}
`

type Plugin struct {
	base.BasePlugin
	config Config

	metadata       Metadata
	tracerProvider *sdktrace.TracerProvider
	route          resource.Route
	service        resource.Service
	stopOnce       sync.Once
}

type Metadata struct {
	TraceIDSource      string                   `json:"trace_id_source,omitempty"`
	Resource           map[string]any           `json:"resource,omitempty"`
	Collector          CollectorConfig          `json:"collector"`
	BatchSpanProcessor BatchSpanProcessorConfig `json:"batch_span_processor"`
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
	Sampler                          SamplerConfig `json:"sampler"`
	AdditionalAttributes             []string      `json:"additional_attributes,omitempty"`
	AdditionalHeaderPrefixAttributes []string      `json:"additional_header_prefix_attributes,omitempty"`
	ServerName                       string        `json:"server_name,omitempty"`
}

type SamplerConfig struct {
	Name    string         `json:"name,omitempty"`
	Options SamplerOptions `json:"options"`
}

type SamplerOptions struct {
	Fraction float64           `json:"fraction,omitempty"`
	Root     RootSamplerConfig `json:"root"`
}

type RootSamplerConfig struct {
	Name    string             `json:"name,omitempty"`
	Options RootSamplerOptions `json:"options"`
}

type RootSamplerOptions struct {
	Fraction float64 `json:"fraction,omitempty"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	effective := p.StaticConfig()
	if effective == nil {
		return fmt.Errorf("effective config is required")
	}
	if p.config.Sampler.Name == "" {
		p.config.Sampler.Name = "always_off"
	}
	if p.config.Sampler.Options.Root.Name == "" {
		p.config.Sampler.Options.Root.Name = "always_off"
	}
	metadata, configured, err := loadMetadata(p.MetadataView(), effective.Config.PluginAttr)
	if err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(metadata.Collector.Address)), "http://") {
		logger.Warn("Using opentelemetry collector.address with no TLS is a security risk")
	}
	p.metadata = metadata

	p.tracerProvider, err = newTracerProvider(p.config.Sampler, metadata, configured)
	if err != nil {
		if errors.Is(err, errUnsupportedMetadata) {
			return err
		}
		p.tracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSampler(buildSampler(p.config.Sampler)))
	}
	return err
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	wrappedNext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetApisixVars(r) == nil {
			r = apisixctx.WithApisixVars(r, nil)
		}
		if apisixctx.GetRequestVars(r) == nil {
			r = apisixctx.WithRequestVars(r)
		}
		requestAttributes := captureHTTPSpanAttributes(r)
		started := time.Now()
		metrics := httpsnoop.CaptureMetrics(next, w, r)
		apisixctx.RegisterRequestVar(r, "$request_time", time.Since(started).Seconds())
		apisixctx.RegisterRequestVar(r, "$bytes_sent", metrics.Written)
		attrs := append(p.resourceSpanAttributes(), requestAttributes...)
		if source, ok := apisixctx.GetRequestVar(r, "$response_source").(string); ok && source != "" {
			attrs = append(attrs, attribute.String("apisix.response_source", source))
		}
		attrs = append(attrs, p.additionalSpanAttributes(r)...)
		if len(attrs) > 0 {
			trace.SpanFromContext(r.Context()).SetAttributes(attrs...)
		}
	})
	opts := []otelchi.Option{
		otelchi.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz"
		}),
		otelchi.WithRequestMethodInSpanName(true),
		otelchi.WithTracerProvider(p.tracerProvider),
	}

	handler := otelchi.Middleware(p.serverName(), opts...)(wrappedNext)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Production route assembly uses RunRequestPhase. Keep Handler as a
		// direct-package adapter and never start a second span when an outer
		// lifecycle or another compatibility wrapper already owns the request.
		if apisixctx.GetRequestLifecycle(r) != nil || r.Context().Value(spanStateContextKey{}) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if p.metadata.TraceIDSource == "x-request-id" {
			requestID := r.Header.Get("X-Request-ID")
			r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
		}
		r = r.WithContext(context.WithValue(r.Context(), spanStateContextKey{}, struct{}{}))
		handler.ServeHTTP(w, r)
	})
}

func captureHTTPSpanAttributes(r *http.Request) []attribute.KeyValue {
	scheme := requestScheme(r)
	attrs := []attribute.KeyValue{
		attribute.String("http.method", r.Method),
		attribute.String("http.scheme", scheme),
		attribute.String("http.target", r.URL.RequestURI()),
		attribute.String("http.request.method", r.Method),
		attribute.String("net.host.name", requestHost(r.Host)),
		attribute.String("url.path", r.URL.Path),
		attribute.String("url.scheme", scheme),
	}
	if userAgent := r.UserAgent(); userAgent != "" {
		attrs = append(attrs,
			attribute.String("http.user_agent", userAgent),
			attribute.String("user_agent.original", userAgent),
		)
	}
	return attrs
}

func requestScheme(r *http.Request) string {
	if r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func requestHost(hostPort string) string {
	if host, _, err := net.SplitHostPort(hostPort); err == nil {
		return host
	}
	return hostPort
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.route = route
	p.service = service
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.tracerProvider == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.tracerProvider.Shutdown(ctx)
	})
}

func (p *Plugin) serverName() string {
	if p.config.ServerName != "" {
		return p.config.ServerName
	}
	return "APISIX"
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
	if value, ok := requestArgumentOrCookie(r, strings.TrimPrefix(key, "$")); ok {
		return value, true
	}
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

func requestArgumentOrCookie(r *http.Request, name string) (string, bool) {
	if argument, ok := strings.CutPrefix(name, "arg_"); ok {
		return nonEmptyValue(r.URL.Query().Get(argument))
	}
	if cookieName, ok := strings.CutPrefix(name, "cookie_"); ok {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			return "", false
		}
		return nonEmptyValue(cookie.Value)
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

func (p *Plugin) requestSpanAttributes(r *http.Request) []attribute.KeyValue {
	attrs := p.resourceSpanAttributes()
	matched := matchedRequestRouteURI(r, p.route)
	if matched == "" {
		return attrs
	}
	for i := range attrs {
		if attrs[i].Key == "http.route" {
			attrs[i] = attribute.String("http.route", matched)
			return attrs
		}
	}
	return append(attrs, attribute.String("http.route", matched))
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

func matchedRequestRouteURI(r *http.Request, route resource.Route) string {
	if r != nil {
		if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
			if pattern := routeContext.RoutePattern(); pattern != "" {
				return pattern
			}
		}
		if value, ok := coerceAttributeValue(apisixctx.GetApisixVar(r, "$matched_uri")); ok {
			return value
		}
	}
	return matchedRouteURI(route)
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

type spanStateContextKey struct{}

type spanState struct {
	span    trace.Span
	started time.Time
	once    sync.Once
}

// RunRequestPhase owns the request-rewrite start. Production route assembly
// invokes this method once; the dynamic end callback is registered on the
// inherited lifecycle so it observes final request/source/outcome values.
func (p *Plugin) RunRequestPhase(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	if r == nil {
		return base.ContinueRequest(r)
	}
	if r.URL.Path == "/healthz" {
		return base.ContinueRequest(r)
	}
	if _, exists := r.Context().Value(spanStateContextKey{}).(*spanState); exists {
		return base.ContinueRequest(r)
	}
	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		r, lifecycle = apisixctx.EnsureRequestLifecycle(r, time.Now())
	}
	if apisixctx.GetApisixVars(r) == nil {
		r = apisixctx.WithApisixVars(r, nil)
	}
	if apisixctx.GetRequestVars(r) == nil {
		r = apisixctx.WithRequestVars(r)
	}
	spanContext := otelapi.GetTextMapPropagator().Extract(
		r.Context(),
		propagation.HeaderCarrier(r.Header),
	)
	if p.metadata.TraceIDSource == "x-request-id" {
		spanContext = context.WithValue(spanContext, requestIDContextKey{}, r.Header.Get("X-Request-ID"))
	}
	started := time.Now()
	spanName := r.Method + " " + r.URL.Path
	if routeURI := matchedRequestRouteURI(r, p.route); routeURI != "" {
		spanName = r.Method + " " + routeURI
	}
	spanContext, span := p.tracer().Start(
		spanContext,
		spanName,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(started),
	)
	state := &spanState{span: span, started: started}
	r = r.WithContext(context.WithValue(spanContext, spanStateContextKey{}, state))
	attrs := append(p.requestSpanAttributes(r), captureHTTPSpanAttributes(r)...)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	if !span.SpanContext().IsSampled() {
		span.End()
		return base.ContinueRequest(r)
	}
	if !lifecycle.AddFinalizer(name, func() error {
		return p.finishSpan(state, lifecycle, r)
	}) {
		state.once.Do(func() { span.End() })
	}
	return base.ContinueRequest(r)
}

func (p *Plugin) tracer() trace.Tracer {
	if p.tracerProvider == nil {
		p.tracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	}
	return p.tracerProvider.Tracer("opentelemetry-lua")
}

func (p *Plugin) finishSpan(state *spanState, lifecycle *apisixctx.RequestLifecycle, fallback *http.Request) error {
	if state == nil || lifecycle == nil {
		return nil
	}
	state.once.Do(func() {
		request := lifecycle.FinalRequest()
		if request == nil {
			request = fallback
		}
		finished := lifecycle.FinishedAt()
		if finished.IsZero() {
			finished = time.Now()
		}
		outcome := lifecycle.Outcome()
		if outcome.Status >= http.StatusInternalServerError {
			state.span.SetStatus(codes.Error, http.StatusText(outcome.Status))
		}
		if request != nil {
			requestTime := finished.Sub(state.started).Seconds()
			if requestTime < 0 {
				requestTime = 0
			}
			apisixctx.RegisterRequestVar(request, "$request_time", requestTime)
			apisixctx.RegisterRequestVar(request, "$bytes_sent", outcome.Bytes)
			attrs := append(p.requestSpanAttributes(request), captureHTTPSpanAttributes(request)...)
			attrs = append(attrs, p.additionalSpanAttributes(request)...)
			attrs = append(attrs,
				attribute.Int("http.status_code", outcome.Status),
				attribute.Int("http.response.status_code", outcome.Status),
			)
			if source := lifecycle.ResponseSource(); source != apisixctx.ResponseSourceUnknown {
				attrs = append(attrs, attribute.String("apisix.response_source", string(source)))
			}
			state.span.SetAttributes(attrs...)
		}
		state.span.End(trace.WithTimestamp(finished))
	})
	return nil
}

type requestIDGenerator struct{}

func (requestIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	traceID := traceIDFromRequestID(ctx.Value(requestIDContextKey{}))
	if !traceID.IsValid() {
		traceID, _ = randomTraceID(rand.Reader)
	}
	spanID, _ := randomSpanID(rand.Reader)
	return traceID, spanID
}

func (requestIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	spanID, _ := randomSpanID(rand.Reader)
	return spanID
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

	return trace.TraceID{}
}

func randomTraceID(reader io.Reader) (trace.TraceID, error) {
	var traceID trace.TraceID
	for !traceID.IsValid() {
		if _, err := io.ReadFull(reader, traceID[:]); err != nil {
			return trace.TraceID{}, fmt.Errorf("generate OpenTelemetry trace ID: %w", err)
		}
	}
	return traceID, nil
}

func randomSpanID(reader io.Reader) (trace.SpanID, error) {
	var spanID trace.SpanID
	for !spanID.IsValid() {
		if _, err := io.ReadFull(reader, spanID[:]); err != nil {
			return trace.SpanID{}, fmt.Errorf("generate OpenTelemetry span ID: %w", err)
		}
	}
	return spanID, nil
}
