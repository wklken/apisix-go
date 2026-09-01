package http_logger

import (
	"context"
	"crypto/tls"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
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

	authHeader      secret.Value
	authHeaderSet   bool
	secretsPrepared bool
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
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("%s metadata decode failed: %w", name, err)
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
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.config.URI)), "http://") {
		logger.Warn("Using http-logger uri with no TLS is a security risk")
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

	processor, err := base.NewBatchProcessor("http logger", p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		RetryDelaySet:      p.config.retryDelaySet,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)
	if err != nil {
		release()
		return err
	}
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

func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	if p.stopped.Swap(true) {
		return
	}
	p.lifecycleMu.RLock()
	processor := p.BatchProcessor
	p.lifecycleMu.RUnlock()
	cleanup := func() {
		p.lifecycleMu.Lock()
		defer p.lifecycleMu.Unlock()
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
		p.client = nil
		p.BatchProcessor = nil
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
		base.ApplySnapshotMatchedRouteFields(fields, snapshot, p.RouteID)
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
		return p.EnqueueLog(fields)
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
	// Stop seals admission first, then asks the batch processor to drain entries
	// that were already accepted. The client remains owned until the processor's
	// terminal cleanup, so those admitted deliveries must stay usable here.
	if p.client == nil {
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
	if p.authHeaderSet {
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
	return secret.ErrCredentialUnavailable
}
