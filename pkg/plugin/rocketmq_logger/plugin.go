package rocketmq_logger

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config        Config
	sender        rocketmqSender
	senderFactory func(*Config) (rocketmqSender, error)

	lifecycleMu sync.RWMutex
	stopped     atomic.Bool

	secretKey       secret.Value
	secretKeySet    bool
	legacySecretKey *store.ResolvedSecret
	secretsPrepared bool
	ready           bool
}

const (
	priority = 402
	name     = "rocketmq-logger"

	originLogKey = "__origin"
)

var producerInstanceSequence atomic.Uint64

const schema = `
{
  "type": "object",
  "properties": {
    "meta_format": {
      "type": "string",
      "default": "default",
      "enum": ["default", "origin"]
    },
    "nameserver_list": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "topic": {
      "type": "string"
    },
    "key": {
      "type": "string"
    },
    "tag": {
      "type": "string"
    },
    "log_format": {
      "type": "object"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "use_tls": {
      "type": "boolean",
      "default": false
    },
    "access_key": {
      "type": "string",
      "default": ""
    },
    "secret_key": {
      "type": "string",
      "default": ""
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
    "inactive_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "buffer_duration": {
      "type": "integer",
      "minimum": 1,
      "default": 60
    },
    "retry_delay": {
      "type": "integer",
      "minimum": 0,
      "default": 1
    },
    "max_retry_count": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  },
  "required": ["nameserver_list", "topic"]
}
`

type Config struct {
	MetaFormat     string            `json:"meta_format,omitempty"`
	NameServerList []string          `json:"nameserver_list"`
	Topic          string            `json:"topic"`
	Key            string            `json:"key,omitempty"`
	Tag            string            `json:"tag,omitempty"`
	LogFormat      map[string]string `json:"log_format,omitempty"`
	Timeout        int               `json:"timeout,omitempty"`
	UseTLS         bool              `json:"use_tls,omitempty"`
	AccessKey      string            `json:"access_key,omitempty"`
	SecretKey      string            `json:"secret_key,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

func (p *Plugin) Stop() {
	if p.stopped.Swap(true) {
		return
	}
	p.lifecycleMu.Lock()
	processor := p.BatchProcessor
	p.lifecycleMu.Unlock()
	cleanup := func() {
		p.lifecycleMu.Lock()
		defer p.lifecycleMu.Unlock()
		shutdownRocketMQSender(p.sender)
		p.sender = nil
		p.senderFactory = nil
		p.BatchProcessor = nil
		if p.legacySecretKey != nil {
			p.legacySecretKey.Destroy()
			p.legacySecretKey = nil
		}
		p.secretKey = secret.Value{}
		p.secretKeySet = false
		p.secretsPrepared = false
		p.ready = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func shutdownRocketMQSender(value rocketmqSender) {
	sender, ok := value.(interface{ Shutdown() error })
	if !ok {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = sender.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func (s *rocketmqClientSender) Shutdown() error {
	return s.producer.Shutdown()
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.InitLogger(p.Send)

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
	if err := validateRocketMQTLS(p.config.UseTLS); err != nil {
		return err
	}
	if p.config.SecretKey == "" {
		p.secretsPrepared = true
		return nil
	}
	value, err := access.Materialize(ctx, "secret_key", p.config.SecretKey)
	if err != nil || value.Use(validateRocketMQSecretKey) != nil {
		return rocketMQSecretKeyUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return rocketMQSecretKeyUnavailable()
	}
	p.secretKey = value
	p.secretKeySet = true
	p.config.SecretKey = descriptor.String()
	p.secretsPrepared = true
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Immutable generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() {
		return secret.ErrCredentialUnavailable
	}
	if p.secretsPrepared {
		return nil
	}
	if err := validateRocketMQTLS(p.config.UseTLS); err != nil {
		return err
	}
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}
	if p.config.SecretKey == "" {
		p.secretsPrepared = true
		return nil
	}
	resolved, err := resolver.ResolveForContext(p.config.SecretKey, name+".secret_key")
	if err != nil {
		return rocketMQSecretKeyUnavailable()
	}
	owner, err := store.MaterializeSecret(resolved)
	if err != nil {
		return rocketMQSecretKeyUnavailable()
	}
	plaintext := owner.Bytes()
	defer clear(plaintext)
	if validateRocketMQSecretKey(string(plaintext)) != nil {
		owner.Destroy()
		return rocketMQSecretKeyUnavailable()
	}
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		owner.Destroy()
		return rocketMQSecretKeyUnavailable()
	}
	p.legacySecretKey = owner
	p.config.SecretKey = descriptor.String()
	p.secretsPrepared = true
	return nil
}

func validateRocketMQTLS(useTLS bool) error {
	if useTLS {
		return fmt.Errorf("%s use_tls is not supported by rocketmq-client-go/v2", name)
	}
	return nil
}

func validateRocketMQSecretKey(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func rocketMQSecretKeyUnavailable() error {
	return fmt.Errorf("%s secret_key: %w", name, secret.ErrCredentialUnavailable)
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
	if err := validateRocketMQTLS(p.config.UseTLS); err != nil {
		return err
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if err := validateBodyExpressions("include_req_body_expr", p.config.IncludeReqBodyExpr); err != nil {
		return err
	}
	if err := validateBodyExpressions("include_resp_body_expr", p.config.IncludeRespBodyExpr); err != nil {
		return err
	}

	p.applyDefaults()

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	sender := p.sender
	createdSender := false
	if sender == nil {
		if err := p.withPrivateConfigLocked(func(config *Config) error {
			factory := p.senderFactory
			if factory == nil {
				factory = p.newSender
			}
			var err error
			sender, err = factory(config)
			return err
		}); err != nil {
			return err
		}
		p.senderFactory = nil
		createdSender = true
	}

	processor := base.NewBatchProcessor("rocketmq logger", base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
		PluginID:           name,
	}, p.RouteID, p.ServerAddr, p.sendBatchFromProcessor)
	if p.stopped.Load() {
		processor.Stop()
		if createdSender {
			shutdownRocketMQSender(sender)
		}
		return secret.ErrCredentialUnavailable
	}
	p.sender = sender
	p.BatchProcessor = processor
	p.ready = true
	return nil
}

func (p *Plugin) withPrivateConfigLocked(use func(*Config) error) error {
	if use == nil || p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	config := Config{
		NameServerList: append([]string(nil), p.config.NameServerList...),
		Timeout:        p.config.Timeout,
		AccessKey:      p.config.AccessKey,
	}
	defer func() { config = Config{} }()
	if p.secretKeySet {
		return p.secretKey.Use(func(secretKey string) error {
			config.SecretKey = secretKey
			defer func() { config.SecretKey = "" }()
			return use(&config)
		})
	}
	if p.legacySecretKey != nil {
		plaintext := p.legacySecretKey.Bytes()
		defer clear(plaintext)
		if len(plaintext) == 0 {
			return secret.ErrCredentialUnavailable
		}
		config.SecretKey = string(plaintext)
		defer func() { config.SecretKey = "" }()
	}
	return use(&config)
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
		if p.config.MetaFormat == "origin" {
			_ = p.enqueueRocketMQLogIfRunning(map[string]any{
				originLogKey: buildOriginRequestLog(r, requestBody, p.config.IncludeReqBody),
			})
			return
		}

		status := metrics.Code
		var logFields map[string]any
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		} else {
			logFields = p.defaultLogFields(r, metrics)
		}
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyDecoded(
				p.config.MaxRespBodyBytes,
				w.Header().Get("Content-Encoding"),
			)
		}

		_ = p.enqueueRocketMQLogIfRunning(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if p.config.MetaFormat == "origin" {
		body := ""
		if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
			body = base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes)
		}
		return p.enqueueRocketMQLogIfRunning(map[string]any{originLogKey: rocketSnapshotOrigin(snapshot, body)})
	}
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	} else {
		fields = rocketSnapshotDefaultFields(p, snapshot)
	}
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
	return p.enqueueRocketMQLogIfRunning(fields)
}

func (p *Plugin) enqueueRocketMQLogIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || !p.ready {
		return base.ErrLogQueueUnavailable
	}
	if p.BatchProcessor == nil {
		return p.Fire(fields)
	}
	return p.EnqueueLog(fields)
}

func rocketSnapshotDefaultFields(p *Plugin, snapshot base.LogSnapshot) map[string]any {
	host := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_ip"))
	port := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_port"))
	upstream := host
	if host != "" && port != "" {
		upstream = net.JoinHostPort(host, port)
	}
	routeID := p.RouteID
	if routeID == "" {
		routeID = fmt.Sprint(base.SnapshotValue(snapshot, "$route_id"))
	}
	return map[string]any{
		"route_id":   routeID,
		"service_id": base.SnapshotValue(snapshot, "$service_id"),
		"client_ip":  base.RemoteIP(snapshot.Request.RemoteAddr),
		"upstream":   upstream,
		"request":    map[string]any{"method": snapshot.Request.Method, "uri": rocketSnapshotURI(snapshot)},
		"response":   map[string]any{"status": snapshot.Outcome.Status, "size": snapshot.Outcome.Bytes},
	}
}

func rocketSnapshotOrigin(snapshot base.LogSnapshot, body string) string {
	var b strings.Builder
	proto := snapshot.Request.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	fmt.Fprintf(&b, "%s %s %s\r\n", snapshot.Request.Method, rocketSnapshotURI(snapshot), proto)
	names := make([]string, 0, len(snapshot.Request.Header))
	for name := range snapshot.Request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range snapshot.Request.Header.Values(name) {
			fmt.Fprintf(&b, "%s: %s\r\n", name, value)
		}
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

func rocketSnapshotURI(snapshot base.LogSnapshot) string {
	if snapshot.Request.URI != "" {
		return snapshot.Request.URI
	}
	if snapshot.Request.URL != "" {
		if parsed, err := url.Parse(snapshot.Request.URL); err == nil && parsed.RequestURI() != "" {
			return parsed.RequestURI()
		}
	}
	return "/"
}

func (p *Plugin) defaultLogFields(r *http.Request, metrics httpsnoop.Metrics) map[string]any {
	upstreamHost := base.RequestVar(r, "$balancer_ip", metrics.Code)
	upstreamPort := base.RequestVar(r, "$balancer_port", metrics.Code)
	upstream := upstreamHost
	if upstreamHost != "" && upstreamPort != "" {
		upstream = net.JoinHostPort(upstreamHost, upstreamPort)
	}
	return map[string]any{
		"route_id":   p.RouteID,
		"service_id": base.RequestVar(r, "$service_id", metrics.Code),
		"client_ip":  base.RemoteIP(r.RemoteAddr),
		"upstream":   upstream,
		"request": map[string]any{
			"method": r.Method,
			"uri":    r.URL.RequestURI(),
		},
		"response": map[string]any{
			"status": metrics.Code,
			"size":   metrics.Written,
		},
	}
}

func validateBodyExpressions(field string, expressions [][]any) error {
	for _, condition := range expressions {
		if len(condition) != 3 {
			return fmt.Errorf("failed to validate the %q expression: each condition must contain 3 items", field)
		}
		operator, ok := condition[1].(string)
		if !ok {
			return fmt.Errorf("failed to validate the %q expression: operator must be a string", field)
		}
		switch operator {
		case "==", "!=", ">", ">=", "<", "<=", "in":
		case "~", "!~":
			pattern, ok := condition[2].(string)
			if !ok {
				return fmt.Errorf("failed to validate the %q expression: regex pattern must be a string", field)
			}
			if _, err := regexp.Compile(pattern); err != nil {
				return fmt.Errorf("failed to validate the %q expression: invalid regex: %w", field, err)
			}
		default:
			return fmt.Errorf("failed to validate the %q expression: invalid operator %q", field, operator)
		}
	}
	return nil
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
	allowRetired bool,
) (int, error) {
	message, err := encodeRocketMQBatch(entries, batchMaxSize)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal rocketmq log message: %w", err)
	}
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.sender == nil || (p.stopped.Load() && !allowRetired) {
		return 0, secret.ErrCredentialUnavailable
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.config.Timeout)*time.Second)
	defer cancel()

	err = p.sender.Send(ctx, rocketmqMessage{
		Topic: p.config.Topic,
		Key:   p.config.Key,
		Tag:   p.config.Tag,
		Body:  message,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to send data to RocketMQ topic %s: %w", p.config.Topic, err)
	}
	return 0, nil
}

func encodeRocketMQBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	return base.EncodeLogBatch(entries, batchMaxSize, originLogKey)
}

func buildOriginRequestLog(r *http.Request, requestBody string, includeReqBody bool) string {
	var b strings.Builder
	requestURI := r.URL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	_, _ = fmt.Fprintf(&b, "%s %s %s\r\n", r.Method, requestURI, r.Proto)

	headerNames := make([]string, 0, len(r.Header))
	for name := range r.Header {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		for _, value := range r.Header.Values(name) {
			_, _ = fmt.Fprintf(&b, "%s: %s\r\n", name, value)
		}
	}

	b.WriteString("\r\n")
	if includeReqBody {
		b.WriteString(requestBody)
	}
	return b.String()
}

func (p *Plugin) applyDefaults() {
	if p.config.MetaFormat == "" {
		p.config.MetaFormat = "default"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
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
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
}
