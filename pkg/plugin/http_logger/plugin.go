package http_logger

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

const (
	// version  = "0.1"
	priority = 410
	name     = "http-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "uri": {
		"type": "string",
		"format": "uri"
	  },
	  "auth_header": {
		"type": "string"
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 3
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
	  "concat_method": {
		"type": "string",
		"default": "json",
		"enum": ["json", "new_line"]
	  },
	  "ssl_verify": {
		"type": "boolean",
		"default": false
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
		"minimum": 1
	  }
	},
	"required": ["uri"]
}`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object"
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  }
}`

type pluginMetadata struct {
	LogFormat         map[string]any `json:"log_format"`
	MaxPendingEntries int            `json:"max_pending_entries,omitempty"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client      *resty.Client
	logFormat   map[string]any
	routeLabels map[string]any

	clientRelease func()

	lifecycleMu sync.RWMutex
	stopped     atomic.Bool

	authHeader       secret.Value
	authHeaderSet    bool
	legacyAuthHeader *store.ResolvedSecret
	secretsPrepared  bool
}

type Config struct {
	URI                 string         `json:"uri"`
	AuthHeader          *string        `json:"auth_header,omitempty"`
	Timeout             int            `json:"timeout"`
	LogFormat           map[string]any `json:"log_format,omitempty"`
	SslVerify           bool           `json:"ssl_verify"`
	MaxReqBodyBytes     int            `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int            `json:"max_resp_body_bytes,omitempty"`
	IncludeReqBody      bool           `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any          `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool           `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any          `json:"include_resp_body_expr,omitempty"`

	// NOTE: not needed
	ConcatMethod string `json:"concat_method"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`

	retryDelaySet bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type config Config

	var parsed struct {
		config
		RetryDelay json.RawMessage `json:"retry_delay"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*c = Config(parsed.config)
	if len(parsed.RetryDelay) > 0 {
		c.retryDelaySet = true
		if string(parsed.RetryDelay) != "null" {
			if err := json.Unmarshal(parsed.RetryDelay, &c.RetryDelay); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) SetResourceContext(route resource.Route, _ resource.Service) {
	p.routeLabels = maps.Clone(route.Labels)
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

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
	if p.config.AuthHeader == nil || *p.config.AuthHeader == "" {
		p.secretsPrepared = true
		return nil
	}

	value, err := access.Materialize(ctx, "auth_header", *p.config.AuthHeader)
	if err != nil || value.Use(validateHTTPAuthorization) != nil {
		return httpAuthorizationUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return httpAuthorizationUnavailable()
	}
	public := descriptor.String()
	p.authHeader = value
	p.authHeaderSet = true
	p.config.AuthHeader = &public
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
	if p.config.AuthHeader == nil || *p.config.AuthHeader == "" {
		p.secretsPrepared = true
		return nil
	}
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}
	resolved, err := resolver.ResolveForContext(*p.config.AuthHeader, name+".auth_header")
	if err != nil {
		return httpAuthorizationUnavailable()
	}
	owner, err := store.MaterializeSecret(resolved)
	if err != nil {
		return httpAuthorizationUnavailable()
	}
	plaintext := owner.Bytes()
	defer clear(plaintext)
	if err := validateHTTPAuthorization(string(plaintext)); err != nil {
		owner.Destroy()
		return httpAuthorizationUnavailable()
	}
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		owner.Destroy()
		return httpAuthorizationUnavailable()
	}
	public := descriptor.String()
	p.legacyAuthHeader = owner
	p.config.AuthHeader = &public
	p.secretsPrepared = true
	return nil
}

func validateHTTPAuthorization(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func httpAuthorizationUnavailable() error {
	return fmt.Errorf("%s auth_header: %w", name, secret.ErrCredentialUnavailable)
}

func (p *Plugin) PostInit() error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	p.config.IncludeReqBodyExpr = normalizeBodyExpression(p.config.IncludeReqBodyExpr)
	p.config.IncludeRespBodyExpr = normalizeBodyExpression(p.config.IncludeRespBodyExpr)
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if err := validateBodyExpression("include_req_body_expr", p.config.IncludeReqBodyExpr); err != nil {
		return err
	}
	if err := validateBodyExpression("include_resp_body_expr", p.config.IncludeRespBodyExpr); err != nil {
		return err
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.ConcatMethod == "" {
		p.config.ConcatMethod = "json"
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
	if p.config.RetryDelay == 0 && !p.config.retryDelaySet {
		p.config.RetryDelay = int(logger_batch.DefaultRetryDelay / time.Second)
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = int(logger_batch.DefaultBufferDuration / time.Second)
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = int(logger_batch.DefaultInactiveTimeout / time.Second)
	}

	// client
	configUID := shared.NewConfigUID()
	client := resty.New()

	configUID.Add(p.config.Timeout)
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	configUID.Add(p.config.SslVerify)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	configUID.Add(p.config.ConcatMethod)
	if p.config.ConcatMethod == "json" {
		client.SetHeader("content-type", "application/json")
	} else {
		client.SetHeader("content-type", "text/plain")
	}
	client.SetHeader("User-Agent", "apisix-go-plugin-http-logger")

	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	sharedClient := value.(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) == 0 {
		p.logFormat = metadata.LogFormat
	} else {
		p.logFormat = p.config.LogFormat
	}
	if p.logFormat != nil {
		var truncated bool
		p.logFormat, truncated = base.TruncateLogFormat(p.logFormat, 5)
		if truncated {
			logger.Warn("log_format nesting exceeds max depth 5, truncating")
		}
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)
	p.SetSnapshotLogFormat(p.logFormat, nil)

	processor := base.NewBatchProcessor("http logger", base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		RetryDelaySet:      p.config.retryDelaySet,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)
	if p.stopped.Load() {
		processor.Stop()
		release()
		return secret.ErrCredentialUnavailable
	}
	p.client = sharedClient
	p.clientRelease = release
	p.BatchProcessor = processor

	return nil
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
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
		p.client = nil
		p.BatchProcessor = nil
		if p.legacyAuthHeader != nil {
			p.legacyAuthHeader.Destroy()
			p.legacyAuthHeader = nil
		}
		p.authHeader = secret.Value{}
		p.authHeaderSet = false
		p.secretsPrepared = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func normalizeBodyExpression(expression []any) []any {
	normalized := make([]any, len(expression))
	for index, item := range expression {
		condition, ok := item.([]any)
		if !ok || len(condition) != 3 || fmt.Sprint(condition[1]) != "in" {
			normalized[index] = item
			continue
		}
		values, ok := condition[2].([]any)
		if !ok {
			normalized[index] = item
			continue
		}
		alternatives := make([]string, len(values))
		for valueIndex, value := range values {
			alternatives[valueIndex] = regexp.QuoteMeta(fmt.Sprint(value))
		}
		normalized[index] = []any{
			condition[0],
			"~",
			"^(" + strings.Join(alternatives, "|") + ")$",
		}
	}
	return normalized
}

func validateBodyExpression(name string, expression []any) error {
	for _, item := range expression {
		if logical, ok := item.(string); ok {
			if strings.EqualFold(logical, "AND") || strings.EqualFold(logical, "OR") {
				continue
			}
			return fmt.Errorf("%s has unsupported logical operator %q", name, logical)
		}
		condition, ok := item.([]any)
		if !ok || len(condition) != 3 {
			return fmt.Errorf("%s condition must contain variable, operator, and value", name)
		}
		operator := fmt.Sprint(condition[1])
		switch operator {
		case "==", "!=", ">", ">=", "<", "<=", "~", "!~":
		default:
			return fmt.Errorf("%s has unsupported operator %q", name, operator)
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		captureRequestBody := p.config.IncludeReqBody || logFormatContains(p.logFormat, "$request_body")
		if captureRequestBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadSharedRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.SharedResponseRecorder
		captureResponseBody := p.config.IncludeRespBody ||
			logFormatContains(p.logFormat, "$resp_body") ||
			len(p.logFormat) == 0
		if captureResponseBody {
			recorder = base.GetOrCreateSharedResponseRecorderWithLimit(w, r, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		status := metrics.Code

		var responseBody string
		if recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			responseBody = recorder.BodyDecoded(
				p.config.MaxRespBodyBytes,
				w.Header().Get("Content-Encoding"),
			)
		}

		var logFields map[string]any
		if len(p.logFormat) > 0 {
			logFields = resolveLogFormat(p.logFormat, r, requestBody, responseBody, status, p.routeLabels)
		} else {
			logFields = p.defaultLogFields(r, status)
		}
		if p.config.IncludeReqBody && requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if p.config.IncludeRespBody && responseBody != "" {
			base.NestedLogMap(logFields, "response")["body"] = responseBody
		}

		_ = p.enqueueLogIfRunning(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) defaultLogFields(r *http.Request, status int) map[string]any {
	routeID := base.RequestVar(r, "$route_id", status)
	if routeID == "" {
		routeID = p.RouteID
	}
	if routeID == "" {
		routeID = "no-matched"
	}
	fields := map[string]any{
		"route_id": routeID,
		"request": map[string]any{
			"method": r.Method,
			"uri":    r.URL.RequestURI(),
		},
		"response": map[string]any{"status": status},
	}
	if serviceID := base.RequestVar(r, "$service_id", status); serviceID != "" {
		fields["service_id"] = serviceID
	}
	if consumerName := base.RequestVar(r, "$consumer_name", status); consumerName != "" {
		fields["consumer"] = map[string]any{"username": consumerName}
	}
	return fields
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	policy := p.LogCapturePolicy()
	requestBody := ""
	if base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
		requestBody = base.SnapshotRequestBody(snapshot, policy.RequestBodyBytes)
	}
	responseBody := ""
	if base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) {
		responseBody = base.SnapshotResponseBody(snapshot, policy.ResponseBodyBytes)
	}
	var fields map[string]any
	if len(p.logFormat) > 0 {
		fields = base.ResolveLogFormat(p.logFormat, func(value string) any {
			switch value {
			case "$request_body":
				return requestBody
			case "$resp_body", "$response_body":
				return responseBody
			case "$status":
				return snapshot.Outcome.Status
			case "$a6_route_labels":
				return p.routeLabels
			case "$host":
				return snapshot.Request.Host
			case "$remote_addr":
				return base.RemoteIP(snapshot.Request.RemoteAddr)
			default:
				return base.SnapshotValue(snapshot, value)
			}
		})
	} else {
		fields = p.defaultSnapshotLogFields(snapshot)
	}
	if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) &&
		requestBody != "" {
		base.NestedLogMap(fields, "request")["body"] = requestBody
	}
	if p.config.IncludeRespBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) &&
		responseBody != "" {
		base.NestedLogMap(fields, "response")["body"] = responseBody
	}
	return p.enqueueLogIfRunning(fields)
}

func (p *Plugin) enqueueLogIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() {
		return base.ErrLogQueueUnavailable
	}
	if p.BatchProcessor == nil {
		return p.Fire(fields)
	}
	return p.EnqueueLog(fields)
}

func (p *Plugin) defaultSnapshotLogFields(snapshot base.LogSnapshot) map[string]any {
	routeID := fmt.Sprint(base.SnapshotValue(snapshot, "$route_id"))
	if routeID == "" {
		routeID = p.RouteID
	}
	if routeID == "" {
		routeID = "no-matched"
	}
	return base.BuildAccessLogFromSnapshot(snapshot, routeID, p.ServerAddr)
}

func resolveLogFormat(
	format map[string]any,
	r *http.Request,
	requestBody string,
	responseBody string,
	status int,
	routeLabels map[string]any,
) map[string]any {
	return base.ResolveLogFormat(format, func(value string) any {
		switch value {
		case "$request_body":
			return requestBody
		case "$resp_body":
			return responseBody
		case "$status":
			return status
		case "$a6_route_labels":
			return routeLabels
		case "$host":
			return r.Host
		case "$remote_addr":
			return base.RemoteIP(r.RemoteAddr)
		default:
			return apisixlog.GetField(r, value)
		}
	})
}

func logFormatContains(format map[string]any, variable string) bool {
	for _, value := range format {
		switch typed := value.(type) {
		case map[string]any:
			if logFormatContains(typed, variable) {
				return true
			}
		case string:
			if typed == variable {
				return true
			}
		}
	}
	return false
}

func (p *Plugin) Send(log map[string]any) {
	body, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in http-logger", err)
		return
	}
	defer clear(body)

	if err := p.sendBody(context.Background(), body); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	body, err := p.encodeBatch(entries, batchMaxSize)
	if err != nil {
		return 0, err
	}
	defer clear(body)
	return 0, p.sendBody(ctx, body)
}

func (p *Plugin) encodeBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	if p.config.ConcatMethod == "new_line" && batchMaxSize > 1 {
		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			body, err := json.Marshal(entry)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal http log entry: %w", err)
			}
			lines = append(lines, string(body))
		}
		return []byte(strings.Join(lines, "\n")), nil
	}

	if batchMaxSize == 1 && len(entries) == 1 {
		body, err := json.Marshal(entries[0])
		if err != nil {
			return nil, fmt.Errorf("failed to marshal http log entry: %w", err)
		}
		return body, nil
	}

	body, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal http log entries: %w", err)
	}
	return body, nil
}

func (p *Plugin) sendBody(ctx context.Context, body []byte) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || p.client == nil {
		return secret.ErrCredentialUnavailable
	}
	request := p.client.R().SetContext(ctx).SetBody(body)
	var response *resty.Response
	defer func() {
		request.Header.Del("Authorization")
		request.Body = nil
		if request.RawRequest != nil {
			request.RawRequest.Header.Del("Authorization")
			request.RawRequest.Body = http.NoBody
			request.RawRequest.GetBody = nil
		}
		if response != nil {
			if responseBody := response.Body(); len(responseBody) > 0 {
				clear(responseBody)
				response.SetBody(nil)
			}
			if response.RawResponse != nil {
				response.RawResponse.Header.Del("Authorization")
				response.RawResponse.Request = nil
				response.RawResponse.Body = http.NoBody
			}
			response.Request = nil
			response.RawResponse = nil
		}
	}()

	send := func() error {
		var err error
		response, err = request.Post(p.config.URI)
		return err
	}
	var err error
	if p.authHeaderSet || p.legacyAuthHeader != nil {
		err = p.useAuthorizationLocked(func(authorization string) error {
			request.SetHeader("Authorization", authorization)
			return send()
		})
	} else {
		err = send()
	}
	if err != nil {
		return fmt.Errorf("error while sending data to [%s]: %w", p.config.URI, err)
	}

	if response.StatusCode() >= 400 {
		return fmt.Errorf(
			"server returned status code [%d] uri [%s], body [%s]",
			response.StatusCode(),
			p.config.URI,
			response.String(),
		)
	}
	return nil
}

func (p *Plugin) useAuthorizationLocked(use func(string) error) error {
	if use == nil || p.stopped.Load() || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.authHeaderSet {
		return p.authHeader.Use(use)
	}
	if p.legacyAuthHeader == nil {
		return secret.ErrCredentialUnavailable
	}
	plaintext := p.legacyAuthHeader.Bytes()
	if len(plaintext) == 0 {
		return secret.ErrCredentialUnavailable
	}
	defer clear(plaintext)
	return use(string(plaintext))
}
