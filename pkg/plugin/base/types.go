package base

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	brotlidec "github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

type ConsumerLookup interface {
	ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool)
	ConsumerByID(id string) (resource.Consumer, bool)
	ConsumerGroupByID(id string) (resource.ConsumerGroup, bool)
}

type Dependencies struct {
	Config            *config.EffectiveConfig
	DataEncryption    data_encryption.Resolver
	Secrets           secret.GenerationCapability
	Metadata          runtime.MetadataView
	Consumers         ConsumerLookup
	Tasks             *runtime.TaskOwner
	CompositeChildren CompositeChildPreparer
}

type BasePlugin struct {
	Name           string
	Priority       int
	Schema         string
	MetadataSchema string
	dependencies   Dependencies
}

func (p *BasePlugin) SetDependencies(deps Dependencies) {
	p.dependencies = deps
}

func (p *BasePlugin) StaticConfig() *config.EffectiveConfig {
	return p.dependencies.Config
}

func (p *BasePlugin) DataEncryption() data_encryption.Resolver {
	return p.dependencies.DataEncryption
}

func (p *BasePlugin) ScopedSecrets() secret.GenerationCapability {
	return p.dependencies.Secrets
}

func (p *BasePlugin) MetadataView() runtime.MetadataView {
	return p.dependencies.Metadata
}

func (p *BasePlugin) ConsumerLookup() ConsumerLookup {
	return p.dependencies.Consumers
}

func (p *BasePlugin) TaskOwner() *runtime.TaskOwner {
	return p.dependencies.Tasks
}

func (p *BasePlugin) CompositeChildPreparer() CompositeChildPreparer {
	return p.dependencies.CompositeChildren
}

func (p *BasePlugin) GetName() string {
	return p.Name
}

func (p *BasePlugin) GetPriority() int {
	return p.Priority
}

func (p *BasePlugin) SetPriority(priority int) {
	p.Priority = priority
}

func (p *BasePlugin) GetSchema() string {
	return p.Schema
}

func (p *BasePlugin) GetMetadataSchema() string {
	return p.MetadataSchema
}

const (
	MAX_REQ_BODY  = 524288 // 512 KiB
	MAX_RESP_BODY = 524288 // 512 KiB
)

type BaseLoggerPlugin struct {
	BasePlugin

	FireChan   chan map[string]any
	AsyncBlock bool

	LogFormat map[string]string
	// SnapshotLogFormat is used by plugins whose public log format supports
	// nested values. LogFormat remains the compatibility representation for
	// flat APISIX logger formats.
	SnapshotLogFormat      map[string]any
	SnapshotLogFormatExtra map[string]any
	RequestBodyExpr        any
	ResponseBodyExpr       any
	RequestBodyBytes       int
	ResponseBodyBytes      int

	SendFunc       func(log map[string]any)
	BatchProcessor *logger_batch.Processor
	RouteID        string
	ServerAddr     string
	stopOnce       sync.Once

	IncludeRequestBody  bool
	IncludeResponseBody bool
}

// ErrLogQueueFull is returned when a detached log callback cannot enqueue its
// payload without blocking the request lifecycle.
var ErrLogQueueFull = errors.New("logger batch processor queue full")

// ErrLogQueueUnavailable reports a logger that has not initialized its
// bounded processor yet. It is deliberately distinct from capacity drops so
// callers can surface configuration/lifecycle errors.
var ErrLogQueueUnavailable = errors.New("logger batch processor unavailable")

// LogCapturePolicy returns the body limits configured on the logger. A zero
// limit means that body bytes are not required by this logger.
func (p *BaseLoggerPlugin) LogCapturePolicy() LogCapturePolicy {
	policy := LogCapturePolicyForFormats(
		p.RequestBodyBytes,
		p.ResponseBodyBytes,
		p.SnapshotLogFormat,
		p.SnapshotLogFormatExtra,
		p.LogFormat,
	)
	if p.IncludeRequestBody ||
		policy.RequestBodyBytes > 0 {
		policy.RequestBodyBytes = p.RequestBodyBytes
		if policy.RequestBodyBytes == 0 {
			policy.RequestBodyBytes = MAX_REQ_BODY
		}
	}
	if p.IncludeResponseBody ||
		policy.ResponseBodyBytes > 0 {
		policy.ResponseBodyBytes = p.ResponseBodyBytes
		if policy.ResponseBodyBytes == 0 {
			policy.ResponseBodyBytes = MAX_RESP_BODY
		}
	}
	return policy
}

// LogCapturePolicyForFormats derives bounded body capture requirements from
// the configured logger formats without retaining plugin configuration in the
// detached callback.
func LogCapturePolicyForFormats(requestBytes, responseBytes int, formats ...any) LogCapturePolicy {
	policy := LogCapturePolicy{}
	for _, format := range formats {
		switch typed := format.(type) {
		case map[string]any:
			if snapshotFormatContains(typed, "$request_body") {
				policy.RequestBodyBytes = requestBytes
			}
			if snapshotFormatContains(typed, "$resp_body") ||
				snapshotFormatContains(typed, "$response_body") {
				policy.ResponseBodyBytes = responseBytes
			}
		case map[string]string:
			if snapshotFormatContainsString(typed, "$request_body") {
				policy.RequestBodyBytes = requestBytes
			}
			if snapshotFormatContainsString(typed, "$resp_body") ||
				snapshotFormatContainsString(typed, "$response_body") {
				policy.ResponseBodyBytes = responseBytes
			}
		}
	}
	if policy.RequestBodyBytes == 0 && formatContainsExpression(formats, "$request_body") {
		policy.RequestBodyBytes = MAX_REQ_BODY
	}
	if policy.ResponseBodyBytes == 0 && (formatContainsExpression(formats, "$resp_body") ||
		formatContainsExpression(formats, "$response_body")) {
		policy.ResponseBodyBytes = MAX_RESP_BODY
	}
	return policy
}

func formatContainsExpression(formats []any, expression string) bool {
	for _, format := range formats {
		switch typed := format.(type) {
		case map[string]any:
			if snapshotFormatContains(typed, expression) {
				return true
			}
		case map[string]string:
			if snapshotFormatContainsString(typed, expression) {
				return true
			}
		}
	}
	return false
}

func snapshotFormatContains(format map[string]any, expression string) bool {
	for _, value := range format {
		switch typed := value.(type) {
		case map[string]any:
			if snapshotFormatContains(typed, expression) {
				return true
			}
		case string:
			if typed == expression {
				return true
			}
		}
	}
	return false
}

func snapshotFormatContainsString(format map[string]string, expression string) bool {
	for _, value := range format {
		if value == expression {
			return true
		}
	}
	return false
}

// RunLogPhase resolves fields from a detached snapshot and enqueues them on
// the bounded batch processor. It never falls back to Fire, whose historical
// AsyncBlock behavior can block a request goroutine.
func (p *BaseLoggerPlugin) RunLogPhase(snapshot LogSnapshot) error {
	fields := p.snapshotFields(snapshot)
	if p.IncludeRequestBody && SnapshotExpressionMatches(snapshot, p.RequestBodyExpr) {
		if body := SnapshotRequestBody(snapshot, p.LogCapturePolicy().RequestBodyBytes); body != "" {
			NestedLogMap(fields, "request")["body"] = body
		}
	}
	if p.IncludeResponseBody && SnapshotExpressionMatches(snapshot, p.ResponseBodyExpr) {
		if body := SnapshotResponseBody(snapshot, p.LogCapturePolicy().ResponseBodyBytes); body != "" {
			NestedLogMap(fields, "response")["body"] = body
		}
	}
	return p.EnqueueLog(fields)
}

func (p *BaseLoggerPlugin) snapshotFields(snapshot LogSnapshot) map[string]any {
	if len(p.SnapshotLogFormat) > 0 {
		fields := ResolveLogFormat(p.SnapshotLogFormat, func(value string) any {
			return SnapshotValue(snapshot, value)
		})
		for key, value := range p.SnapshotLogFormatExtra {
			fields[key] = ResolveLogFormat(map[string]any{"value": value}, func(expression string) any {
				return SnapshotValue(snapshot, expression)
			})["value"]
		}
		return fields
	}
	return GetFieldsFromSnapshot(snapshot, p.LogFormat)
}

// EnqueueLog exposes the non-blocking delivery boundary used by detached log
// callbacks. BatchProcessor.Push is the only permitted production path.
func (p *BaseLoggerPlugin) EnqueueLog(entry map[string]any) error {
	return EnqueueLog(p.BatchProcessor, entry)
}

// EnqueueLog is the standalone form for logger implementations that cannot
// embed BaseLoggerPlugin (for example metric and file loggers).
func EnqueueLog(processor *logger_batch.Processor, entry map[string]any) error {
	if processor == nil {
		return ErrLogQueueUnavailable
	}
	if !processor.Push(entry) {
		return ErrLogQueueFull
	}
	return nil
}

// SnapshotValue resolves one access-log expression without consulting a live
// request. Literal values are preserved as strings, matching legacy format
// resolution.
func SnapshotValue(snapshot LogSnapshot, expression string) any {
	if !strings.HasPrefix(expression, "$") {
		return expression
	}
	return log.GetFieldsFromSnapshot(snapshot, map[string]string{"value": expression})["value"]
}

// SnapshotRequestBody returns a bounded detached request body.
func SnapshotRequestBody(snapshot LogSnapshot, limit int) string {
	if limit <= 0 {
		return ""
	}
	return string(boundSnapshotBody(snapshot.Request.Body, limit))
}

// SnapshotResponseBody returns a bounded response body, decoding the same
// gzip/brotli encodings handled by the legacy response recorder.
func SnapshotResponseBody(snapshot LogSnapshot, limit int) string {
	if limit <= 0 || len(snapshot.Response.Body) == 0 {
		return ""
	}
	body := snapshot.Response.Body
	switch strings.ToLower(strings.TrimSpace(snapshot.Response.Header.Get("Content-Encoding"))) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return string(boundSnapshotBody(body, limit))
		}
		decoded, readErr := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
		_ = reader.Close()
		if readErr == nil {
			body = decoded
		}
	case "br":
		decoded, err := io.ReadAll(io.LimitReader(brotlidec.NewReader(bytes.NewReader(body)), int64(limit)+1))
		if err == nil {
			body = decoded
		}
	}
	return string(boundSnapshotBody(body, limit))
}

func boundSnapshotBody(body []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if len(body) > limit {
		return body[:limit]
	}
	return body
}

// SnapshotExpressionMatches preserves the legacy logger expression grammar
// while resolving every variable from the detached snapshot.
func SnapshotExpressionMatches(snapshot LogSnapshot, expressions any) bool {
	requestURL, err := url.Parse(snapshot.Request.URL)
	if err != nil || requestURL == nil {
		requestURL = &url.URL{}
	}
	if snapshot.Request.URI != "" {
		if parsed, parseErr := url.ParseRequestURI(snapshot.Request.URI); parseErr == nil {
			requestURL = parsed
		}
	}
	if requestURL.RawQuery == "" && len(snapshot.Request.Query) > 0 {
		requestURL.RawQuery = snapshot.Request.Query.Encode()
	}
	header := snapshot.Request.Header.Clone()
	if snapshot.Request.Scheme != "" && header.Get("X-Forwarded-Proto") == "" {
		header.Set("X-Forwarded-Proto", snapshot.Request.Scheme)
	}
	request := &http.Request{
		Method:        snapshot.Request.Method,
		URL:           requestURL,
		Header:        header,
		Host:          snapshot.Request.Host,
		RemoteAddr:    snapshot.Request.RemoteAddr,
		Proto:         snapshot.Request.Proto,
		ContentLength: snapshot.Request.ContentLength,
	}
	requestContext := context.WithValue(context.Background(), apisixctx.ApisixVarsKey, snapshot.Request.APISIXVars)
	requestContext = context.WithValue(requestContext, apisixctx.RequestVarsKey, snapshot.Request.RequestVars)
	return ExprMatched(request.WithContext(requestContext), expressions, snapshot.Outcome.Status)
}

func (p *BaseLoggerPlugin) SetRouteContext(routeID string, serverAddr string) {
	p.RouteID = routeID
	p.ServerAddr = serverAddr
}

// SetLogCapturePolicy wires a logger's bounded body policy into the detached
// callback implementation while leaving its legacy Handler untouched.
func (p *BaseLoggerPlugin) SetLogCapturePolicy(
	includeRequest, includeResponse bool,
	requestBytes, responseBytes int,
	requestExpr, responseExpr any,
) {
	p.IncludeRequestBody = includeRequest
	p.IncludeResponseBody = includeResponse
	p.RequestBodyBytes = requestBytes
	p.ResponseBodyBytes = responseBytes
	p.RequestBodyExpr = requestExpr
	p.ResponseBodyExpr = responseExpr
}

func (p *BaseLoggerPlugin) SetSnapshotLogFormat(format, extra map[string]any) {
	p.SnapshotLogFormat = format
	p.SnapshotLogFormatExtra = extra
}

// InitLogger initializes the buffered fire channel, blocking policy and the
// per-plugin Send function.
func (p *BaseLoggerPlugin) InitLogger(send func(map[string]any)) {
	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = send
}

// BatchDefaults carries the per-plugin batch configuration values in seconds.
type BatchDefaults struct {
	BatchMaxSize       int
	MaxRetryCount      int
	RetryDelaySec      int
	RetryDelaySet      bool
	BufferDurationSec  int
	InactiveTimeoutSec int
	MaxPendingEntries  int
	PluginID           string

	// Resource overrides are internal until each logger schema exposes them.
	MaxConcurrentDeliveries int
	DeliveryTimeoutSec      int
	ShutdownTimeoutSec      int
}

// ApplyBatchDefaults fills zero batch values with logger_batch defaults.
// RetryDelaySec is only defaulted when RetryDelaySet is false.
func ApplyBatchDefaults(d *BatchDefaults) {
	if d.BatchMaxSize <= 0 {
		d.BatchMaxSize = logger_batch.DefaultBatchMaxSize
	}
	if d.RetryDelaySec == 0 && !d.RetryDelaySet {
		d.RetryDelaySec = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if d.BufferDurationSec <= 0 {
		d.BufferDurationSec = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if d.InactiveTimeoutSec <= 0 {
		d.InactiveTimeoutSec = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}
	if d.MaxPendingEntries <= 0 {
		d.MaxPendingEntries = logger_batch.DefaultMaxPendingEntries
	}
	if d.MaxConcurrentDeliveries <= 0 {
		d.MaxConcurrentDeliveries = logger_batch.DefaultMaxConcurrentDeliveries
	}
	if d.MaxConcurrentDeliveries > 8 {
		d.MaxConcurrentDeliveries = 8
	}
	if d.DeliveryTimeoutSec <= 0 {
		d.DeliveryTimeoutSec = int(logger_batch.DefaultDeliveryTimeout / time.Second)
	}
	if d.ShutdownTimeoutSec <= 0 {
		d.ShutdownTimeoutSec = int(logger_batch.DefaultShutdownTimeout / time.Second)
	}
}

// NewBatchProcessor constructs a logger batch processor from second-based
// batch defaults.
func NewBatchProcessor(
	name string,
	d BatchDefaults,
	routeID, serverAddr string,
	deliver logger_batch.ContextDeliveryFunc,
) *logger_batch.Processor {
	ApplyBatchDefaults(&d)
	return logger_batch.NewWithContext(logger_batch.Config{
		Name:                    name,
		BatchMaxSize:            d.BatchMaxSize,
		MaxRetryCount:           d.MaxRetryCount,
		RetryDelay:              time.Duration(d.RetryDelaySec) * time.Second,
		RetryDelaySet:           d.RetryDelaySet,
		BufferDuration:          time.Duration(d.BufferDurationSec) * time.Second,
		InactiveTimeout:         time.Duration(d.InactiveTimeoutSec) * time.Second,
		MaxPendingEntries:       d.MaxPendingEntries,
		PluginID:                d.PluginID,
		MaxConcurrentDeliveries: d.MaxConcurrentDeliveries,
		DeliveryTimeout:         time.Duration(d.DeliveryTimeoutSec) * time.Second,
		ShutdownTimeout:         time.Duration(d.ShutdownTimeoutSec) * time.Second,
		RouteID:                 routeID,
		ServerAddr:              serverAddr,
	}, deliver)
}

func (p *BaseLoggerPlugin) Stop() {
	p.StopWithCleanup(nil)
}

// StopWithCleanup retains sink resources until every batch delivery callback
// has returned, while preserving the processor's bounded caller-facing stop.
func (p *BaseLoggerPlugin) StopWithCleanup(cleanup func()) {
	p.stopOnce.Do(func() {
		if p.BatchProcessor != nil {
			p.BatchProcessor.StopWithCleanup(cleanup)
		} else if cleanup != nil {
			cleanup()
		}
	})
}

func (p *BaseLoggerPlugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		logFields := log.GetFields(r, p.LogFormat)

		// FIXME: if not LogFormat, will get full log,
		// reference: https://github.com/apache/apisix/blob/master/apisix/utils/log-util.lua#L136

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *BaseLoggerPlugin) Fire(entry map[string]any) error {
	if p.BatchProcessor != nil {
		p.BatchProcessor.Push(entry)
		return nil
	}

	select {
	case p.FireChan <- entry: // try and put into chan, if fail will to default
	default:
		if p.AsyncBlock {
			logger.Warn("the log buffered chan is full! will block")
			p.FireChan <- entry // Blocks the goroutine because buffer is full.
			return nil
		}
		logger.Warn("the log buffered chan is full! will drop")
		// Drop message by default.
	}
	return nil
}
