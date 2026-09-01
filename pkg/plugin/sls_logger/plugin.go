package sls_logger

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config      Config
	lifecycleMu sync.RWMutex
	stopOnce    sync.Once
	stopped     atomic.Bool

	secretsPrepared bool
	ready           bool
	accessKeySecret *secret.Value

	addr string

	dialTLS func(dialer *net.Dialer, network, address string, config *tls.Config) (net.Conn, error)
}

const (
	priority = 406
	name     = "sls-logger"

	slsQueuedLogEnvelopeKey = "apisix-go:sls-logger:queued-entry"
	slsTimestampLayout      = "2006-01-02T15:04:05.000Z"
)

type slsQueuedLogEntry struct {
	fields   map[string]any
	hostname string
}

const schema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string"
    },
    "port": {
      "type": "integer"
    },
    "project": {
      "type": "string"
    },
    "logstore": {
      "type": "string"
    },
    "access_key_id": {
      "type": "string"
    },
    "access_key_secret": {
      "type": "string"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
    },
    "log_format": {
      "type": "object"
    },
    "include_req_body": {
      "type": "boolean",
      "default": false
    },
    "include_req_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "include_resp_body": {
      "type": "boolean",
      "default": false
    },
    "include_resp_body_expr": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "array"
      }
    },
    "max_req_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "max_resp_body_bytes": {
      "type": "integer",
      "minimum": 1,
      "default": 524288
    },
    "batch_max_size": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "max_retry_count": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "retry_delay": {
      "type": "integer",
      "minimum": 0,
      "default": 1
    },
    "buffer_duration": {
      "type": "integer",
      "minimum": 1,
      "default": 60
    },
    "inactive_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1,
      "default": 10000
    }
  },
  "required": ["host", "port", "project", "logstore", "access_key_id", "access_key_secret"]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object"
    }
  }
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	Project         string            `json:"project"`
	Logstore        string            `json:"logstore"`
	AccessKeyID     string            `json:"access_key_id"`
	AccessKeySecret string            `json:"access_key_secret"`
	SSLVerify       *bool             `json:"ssl_verify,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	LogFormat       map[string]string `json:"log_format,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
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

func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}
	value, err := access.Materialize(ctx, "access_key_secret", p.config.AccessKeySecret)
	if err != nil || value.Use(validateSLSAccessKeySecret) != nil {
		return slsAccessKeySecretUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return slsAccessKeySecretUnavailable()
	}
	p.config.AccessKeySecret = descriptor.String()
	p.accessKeySecret = &value
	p.secretsPrepared = true
	return nil
}

func validateSLSAccessKeySecret(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func slsAccessKeySecretUnavailable() error {
	return fmt.Errorf("%s access_key_secret: %w", name, secret.ErrCredentialUnavailable)
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.ready {
		return nil
	}
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("sls-logger metadata decode failed: %w", err)
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}

	if p.config.SSLVerify == nil {
		verify := false
		p.config.SSLVerify = &verify
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 5000
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
	if p.config.BatchMaxSize == 0 {
		p.config.BatchMaxSize = logger_batch.DefaultBatchMaxSize
	}
	if p.config.RetryDelay == 0 {
		p.config.RetryDelay = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = 10000
	}
	p.addr = net.JoinHostPort(p.config.Host, strconv.Itoa(p.config.Port))

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	processor, err := base.NewBatchProcessor("sls logger", p.TaskOwner(), base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
		PluginID:           name,
	}, p.RouteID, p.ServerAddr, p.sendBatchFromProcessor)
	if err != nil {
		return err
	}
	if p.stopped.Load() {
		processor.Stop()
		return secret.ErrCredentialUnavailable
	}
	p.BatchProcessor = processor
	p.ready = true

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadSharedRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.SharedResponseRecorder
		if p.config.IncludeRespBody {
			recorder = base.GetOrCreateSharedResponseRecorderWithLimit(w, r, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		status := metrics.Code

		var logFields map[string]any
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		} else {
			logFields = defaultAccessLogFields(r, status, w.Header())
		}
		logFields["route_id"] = p.RouteID
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}

		_ = p.enqueueSLSIfRunning(logFields, base.HostWithoutPort(r.Host))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	} else {
		fields = slsSnapshotDefaultFields(snapshot)
	}
	routeID := p.RouteID
	if routeID == "" {
		routeID = fmt.Sprint(base.SnapshotValue(snapshot, "$route_id"))
	}
	fields["route_id"] = routeID
	if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
		if body := base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes); body != "" {
			base.NestedLogMap(fields, "request")["body"] = body
		}
	}
	if p.config.IncludeRespBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) {
		if body := base.SnapshotResponseBody(snapshot, p.config.MaxRespBodyBytes); body != "" {
			base.NestedLogMap(fields, "response")["body"] = body
		}
	}
	return p.enqueueSLSIfRunning(fields, base.HostWithoutPort(snapshot.Request.Host))
}

func (p *Plugin) enqueueSLSIfRunning(fields map[string]any, hostname string) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || !p.ready {
		return base.ErrLogQueueUnavailable
	}
	return p.EnqueueLog(map[string]any{
		slsQueuedLogEnvelopeKey: slsQueuedLogEntry{fields: fields, hostname: hostname},
	})
}

func slsSnapshotDefaultFields(snapshot base.LogSnapshot) map[string]any {
	requestHeaders := base.CollapseAccessLogHeaderValues(snapshot.Request.Header)
	requestHeaders["host"] = snapshot.Request.Host
	responseHeaders := base.CollapseAccessLogHeaderValues(snapshot.Response.Header)
	query := make(map[string]any, len(snapshot.Request.Query))
	for name, values := range snapshot.Request.Query {
		if len(values) == 1 {
			query[name] = values[0]
		} else {
			query[name] = append([]string(nil), values...)
		}
	}
	return map[string]any{
		"client_ip": base.RemoteIP(snapshot.Request.RemoteAddr),
		"request": map[string]any{
			"method": snapshot.Request.Method, "uri": snapshotURI(snapshot),
			"headers": requestHeaders, "querystring": query,
		},
		"response": map[string]any{"status": snapshot.Outcome.Status, "headers": responseHeaders},
	}
}

func snapshotURI(snapshot base.LogSnapshot) string {
	if snapshot.Request.URI != "" {
		return snapshot.Request.URI
	}
	return "/"
}

func defaultAccessLogFields(r *http.Request, status int, responseHeaders http.Header) map[string]any {
	requestHeaders := base.CollapseAccessLogHeaderValues(r.Header)
	requestHeaders["host"] = r.Host
	return map[string]any{
		"client_ip": base.RemoteIP(r.RemoteAddr),
		"request": map[string]any{
			"method":      r.Method,
			"uri":         r.URL.RequestURI(),
			"headers":     requestHeaders,
			"querystring": queryFields(r),
		},
		"response": map[string]any{
			"status":  status,
			"headers": base.CollapseAccessLogHeaderValues(responseHeaders),
		},
	}
}

func queryFields(r *http.Request) map[string]any {
	query := r.URL.Query()
	fields := make(map[string]any, len(query))
	for name, values := range query {
		if len(values) == 1 {
			fields[name] = values[0]
		} else {
			fields[name] = append([]string(nil), values...)
		}
	}
	return fields
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	return p.sendBatch(ctx, entries, batchMaxSize, false)
}

func (p *Plugin) sendBatchFromProcessor(
	ctx context.Context,
	entries []map[string]any,
	batchMaxSize int,
) (int, error) {
	return p.sendBatch(ctx, entries, batchMaxSize, true)
}

func (p *Plugin) sendBatch(
	ctx context.Context,
	entries []map[string]any,
	batchMaxSize int,
	allowRetiredDrain bool,
) (int, error) {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if (!allowRetiredDrain && p.stopped.Load()) || !p.secretsPrepared || !p.ready {
		return 0, secret.ErrCredentialUnavailable
	}
	_ = batchMaxSize

	var sendErr error
	if err := p.useAccessKeySecret(func(accessKeySecret string) error {
		messages := make([]string, 0, len(entries))
		for _, entry := range entries {
			messages = append(messages, p.buildMessageWithSecret(entry, accessKeySecret))
		}
		sendErr = p.sendMessage(ctx, strings.Join(messages, ""))
		return nil
	}); err != nil {
		return 0, secret.ErrCredentialUnavailable
	}
	return 0, sendErr
}

// Stop prevents new work, drains pending batch entries, then drops all
// generation readiness state and its private access-key material.
func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		p.lifecycleMu.RLock()
		processor := p.BatchProcessor
		p.lifecycleMu.RUnlock()
		cleanup := func() {
			p.lifecycleMu.Lock()
			defer p.lifecycleMu.Unlock()
			p.BatchProcessor = nil
			p.secretsPrepared = false
			p.ready = false
			p.accessKeySecret = nil
		}
		if processor != nil {
			processor.StopWithCleanup(cleanup)
		} else {
			cleanup()
		}
	})
}

func (p *Plugin) sendMessage(ctx context.Context, message string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Duration(p.config.Timeout) * time.Millisecond)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	operationTimeout := time.Until(deadline)
	if operationTimeout <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	dialer := &net.Dialer{Timeout: operationTimeout}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: p.config.Host,
	}
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicit APISIX-compatible operator opt-out
	}
	dialTLS := p.dialTLS
	var conn net.Conn
	var err error
	if dialTLS != nil {
		conn, err = dialTLS(dialer, "tcp", p.addr, tlsConfig)
	} else {
		handshakeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
		defer cancel()
		raw, dialErr := dialer.DialContext(handshakeCtx, "tcp", p.addr)
		if dialErr != nil {
			return fmt.Errorf("failed to connect to SLS TLS endpoint %s: %w", p.addr, dialErr)
		}
		tlsConn := tls.Client(raw, tlsConfig)
		if handshakeErr := tlsConn.HandshakeContext(handshakeCtx); handshakeErr != nil {
			_ = raw.Close()
			return fmt.Errorf("failed to connect to SLS TLS endpoint %s: %w", p.addr, handshakeErr)
		}
		conn = tlsConn
	}
	if err != nil {
		return fmt.Errorf("failed to connect to SLS TLS endpoint %s: %w", p.addr, err)
	}
	defer func() { _ = conn.Close() }()
	stopWatcher := watchConnectionCancellation(ctx, conn)
	defer stopWatcher()

	if err := conn.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("failed to set SLS write deadline: %w", err)
	}

	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("failed to send SLS log message: %w", err)
	}
	return nil
}

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		_ = conn.Close()
	})
	return func() {
		if stop() {
			close(done)
		}
		<-done
	}
}

func (p *Plugin) buildMessage(log map[string]any) string {
	var message string
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	_ = p.useAccessKeySecret(func(accessKeySecret string) error {
		message = p.buildMessageWithSecret(log, accessKeySecret)
		return nil
	})
	return message
}

func (p *Plugin) buildMessageWithSecret(log map[string]any, accessKeySecret string) string {
	hostname := base.Hostname()
	if queued, ok := log[slsQueuedLogEnvelopeKey].(slsQueuedLogEntry); ok {
		log = queued.fields
		hostname = queued.hostname
	}
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}

	if hostname == "" {
		hostname = "-"
	}

	return strings.Join([]string{
		"<46>1",
		time.Now().UTC().Format(slsTimestampLayout),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(accessKeySecret),
		string(payload),
	}, " ") + "\n"
}

func (p *Plugin) structuredData(accessKeySecret string) string {
	return fmt.Sprintf(
		`[logservice project="%s" logstore="%s" access-key-id="%s" access-key-secret="%s"]`,
		escapeStructuredDataValue(p.config.Project),
		escapeStructuredDataValue(p.config.Logstore),
		escapeStructuredDataValue(p.config.AccessKeyID),
		escapeStructuredDataValue(accessKeySecret),
	)
}

func (p *Plugin) useAccessKeySecret(use func(string) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	if p.accessKeySecret != nil {
		return p.accessKeySecret.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

func escapeStructuredDataValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `]`, `\]`)
	return value
}
