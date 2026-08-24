package splunk_hec_logging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client         *resty.Client
	logFormatExtra map[string]string

	clientRelease func()
	lifecycleMu   sync.RWMutex
	stopped       atomic.Bool

	token           secret.Value
	tokenSet        bool
	legacyToken     *store.ResolvedSecret
	secretsPrepared bool
	ready           bool
}

const (
	priority = 409
	name     = "splunk-hec-logging"

	defaultSource     = "apache-apisix-splunk-hec-logging"
	defaultSourceType = "_json"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint": {
      "type": "object",
      "properties": {
        "uri": {
          "type": "string",
          "format": "uri"
        },
        "token": {
          "type": "string"
        },
        "channel": {
          "type": "string"
        },
        "timeout": {
          "type": "integer",
          "minimum": 1,
          "default": 10
        },
        "keepalive_timeout": {
          "type": "integer",
          "minimum": 1000,
          "default": 60000
        }
      },
      "required": ["uri", "token"]
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "log_format": {
      "type": "object"
    },
    "log_format_extra": {
      "type": "object"
    },
    "name": {
      "type": "string"
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
  "required": ["endpoint"]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object"
    },
    "log_format_extra": {
      "type": "object"
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  }
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	LogFormatExtra    map[string]string `json:"log_format_extra"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Endpoint struct {
	URI              string `json:"uri"`
	Token            string `json:"token"`
	Channel          string `json:"channel,omitempty"`
	Timeout          int    `json:"timeout,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
}

type Config struct {
	Endpoint       Endpoint          `json:"endpoint"`
	SSLVerify      *bool             `json:"ssl_verify,omitempty"`
	LogFormat      map[string]string `json:"log_format,omitempty"`
	LogFormatExtra map[string]string `json:"log_format_extra,omitempty"`

	Name              string `json:"name,omitempty"`
	BatchMaxSize      int    `json:"batch_max_size,omitempty"`
	InactiveTimeout   int    `json:"inactive_timeout,omitempty"`
	BufferDuration    int    `json:"buffer_duration,omitempty"`
	RetryDelay        int    `json:"retry_delay,omitempty"`
	MaxRetryCount     int    `json:"max_retry_count,omitempty"`
	MaxPendingEntries int    `json:"max_pending_entries,omitempty"`
}

type splunkEvent struct {
	Time       float64        `json:"time"`
	Host       string         `json:"host"`
	Source     string         `json:"source"`
	SourceType string         `json:"sourcetype"`
	Event      map[string]any `json:"event"`
}

func (p *Plugin) Config() any {
	return &p.config
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
	value, err := access.Materialize(ctx, "endpoint.token", p.config.Endpoint.Token)
	if err != nil || value.Use(validateSplunkToken) != nil {
		return splunkTokenUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return splunkTokenUnavailable()
	}
	p.token = value
	p.tokenSet = true
	p.config.Endpoint.Token = descriptor.String()
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
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}
	resolved, err := resolver.ResolveForContext(p.config.Endpoint.Token, name+".endpoint.token")
	if err != nil {
		return splunkTokenUnavailable()
	}
	owner, err := store.MaterializeSecret(resolved)
	if err != nil {
		return splunkTokenUnavailable()
	}
	plaintext := owner.Bytes()
	defer clear(plaintext)
	if validateSplunkToken(string(plaintext)) != nil {
		owner.Destroy()
		return splunkTokenUnavailable()
	}
	digest := sha256.Sum256(plaintext)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		owner.Destroy()
		return splunkTokenUnavailable()
	}
	p.legacyToken = owner
	p.config.Endpoint.Token = descriptor.String()
	p.secretsPrepared = true
	return nil
}

func validateSplunkToken(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func splunkTokenUnavailable() error {
	return fmt.Errorf("%s endpoint.token: %w", name, secret.ErrCredentialUnavailable)
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

	if p.config.Endpoint.Timeout == 0 {
		p.config.Endpoint.Timeout = 10
	}
	if p.config.Endpoint.KeepaliveTimeout == 0 {
		p.config.Endpoint.KeepaliveTimeout = 60000
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

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Endpoint.URI)
	configUID.Add(p.config.Endpoint.Channel)
	configUID.Add(p.config.Endpoint.Timeout)
	configUID.Add(p.config.Endpoint.KeepaliveTimeout)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Endpoint.Timeout) * time.Second)
	client.SetHeader("Content-Type", "application/json")
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	if p.config.Endpoint.Channel != "" {
		client.SetHeader("X-Splunk-Request-Channel", p.config.Endpoint.Channel)
	}
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
	switch {
	case p.config.LogFormat != nil:
		p.LogFormat = p.config.LogFormat
	case metadata.LogFormat != nil:
		p.LogFormat = metadata.LogFormat
	default:
		if p.config.LogFormatExtra != nil {
			p.logFormatExtra = p.config.LogFormatExtra
		} else {
			p.logFormatExtra = metadata.LogFormatExtra
		}
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	processor := base.NewBatchProcessor(name, base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.sendBatchFromProcessor)
	if p.stopped.Load() {
		processor.Stop()
		release()
		return secret.ErrCredentialUnavailable
	}
	p.client = sharedClient
	p.clientRelease = release
	p.BatchProcessor = processor
	p.ready = true
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
		if p.legacyToken != nil {
			p.legacyToken.Destroy()
			p.legacyToken = nil
		}
		p.token = secret.Value{}
		p.tokenSet = false
		p.secretsPrepared = false
		p.ready = false
	}
	if processor != nil {
		processor.StopWithCleanup(cleanup)
	} else {
		cleanup()
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := captureRequest(r)
		metrics := httpsnoop.CaptureMetrics(next, w, r)

		var logFields map[string]any
		if p.LogFormat != nil {
			logFields = base.ResolveStringLogFormat(p.LogFormat, func(value string) any {
				return p.resolveLogFormatValue(r, value, request.host, request.remoteAddr)
			})
		} else {
			logFields = buildDefaultEvent(request, w.Header(), r, metrics)
			for key, value := range p.logFormatExtra {
				if _, exists := logFields[key]; !exists {
					logFields[key] = p.resolveLogFormatValue(r, value, request.host, request.remoteAddr)
				}
			}
		}
		_ = p.enqueueSplunkIfRunning(logFields)
	})
}

func (p *Plugin) LogCapturePolicy() base.LogCapturePolicy {
	policy := p.BaseLoggerPlugin.LogCapturePolicy()
	formatted := base.LogCapturePolicyForFormats(
		p.RequestBodyBytes,
		p.ResponseBodyBytes,
		p.logFormatExtra,
	)
	policy.RequestBodyBytes = max(policy.RequestBodyBytes, formatted.RequestBodyBytes)
	policy.ResponseBodyBytes = max(policy.ResponseBodyBytes, formatted.ResponseBodyBytes)
	return policy
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	var fields map[string]any
	if p.LogFormat != nil {
		fields = base.ResolveStringLogFormat(p.LogFormat, func(value string) any {
			switch value {
			case "$host":
				return base.RemoteIP(snapshot.Request.Host)
			case "$remote_addr":
				return base.RemoteIP(snapshot.Request.RemoteAddr)
			case "$upstream_unresolved_host":
				return base.SnapshotValue(snapshot, "$balancer_ip")
			default:
				return base.SnapshotValue(snapshot, value)
			}
		})
	} else {
		fields = splunkSnapshotDefaultEvent(snapshot)
		for key, value := range p.logFormatExtra {
			if _, exists := fields[key]; !exists {
				fields[key] = base.SnapshotValue(snapshot, value)
			}
		}
	}
	return p.enqueueSplunkIfRunning(fields)
}

func (p *Plugin) enqueueSplunkIfRunning(fields map[string]any) error {
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

func splunkSnapshotDefaultEvent(snapshot base.LogSnapshot) map[string]any {
	requestSize := max(snapshot.Request.ContentLength, 0)
	latency := int64(0)
	if !snapshot.Started.IsZero() && !snapshot.Finished.IsZero() {
		latency = snapshot.Finished.Sub(snapshot.Started).Milliseconds()
	}
	upstreamHost := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_ip"))
	upstreamPort := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_port"))
	upstream := upstreamHost
	if upstreamHost != "" && upstreamPort != "" {
		upstream = net.JoinHostPort(upstreamHost, upstreamPort)
	}
	requestURL := snapshot.Request.URL
	if parsed, err := url.Parse(requestURL); err != nil || !parsed.IsAbs() {
		scheme := snapshot.Request.Scheme
		if scheme == "" {
			scheme = "http"
		}
		requestURL = scheme + "://" + snapshot.Request.Host + snapshot.Request.URI
	}
	return map[string]any{
		"request_url":      requestURL,
		"request_method":   snapshot.Request.Method,
		"request_headers":  base.CollapseAccessLogHeaderValues(snapshot.Request.Header),
		"request_query":    snapshot.Request.Query,
		"request_size":     requestSize,
		"response_headers": base.CollapseAccessLogHeaderValues(snapshot.Response.Header),
		"response_status":  snapshot.Outcome.Status,
		"response_size":    snapshot.Outcome.Bytes,
		"latency":          latency,
		"upstream":         upstream,
	}
}

type requestSnapshot struct {
	url        string
	method     string
	headers    http.Header
	query      map[string][]string
	size       int64
	host       string
	remoteAddr string
}

func captureRequest(r *http.Request) requestSnapshot {
	size := max(r.ContentLength, 0)
	host := base.RemoteIP(r.Host)
	return requestSnapshot{
		url:        base.RequestVar(r, "$scheme", 0) + "://" + r.Host + r.URL.RequestURI(),
		method:     r.Method,
		headers:    r.Header.Clone(),
		query:      map[string][]string(r.URL.Query()),
		size:       size,
		host:       host,
		remoteAddr: base.RemoteIP(r.RemoteAddr),
	}
}

func buildDefaultEvent(
	request requestSnapshot,
	responseHeaders http.Header,
	r *http.Request,
	metrics httpsnoop.Metrics,
) map[string]any {
	return map[string]any{
		"request_url":      request.url,
		"request_method":   request.method,
		"request_headers":  base.CollapseAccessLogHeaderValues(request.headers),
		"request_query":    request.query,
		"request_size":     request.size,
		"response_headers": base.CollapseAccessLogHeaderValues(responseHeaders),
		"response_status":  metrics.Code,
		"response_size":    metrics.Written,
		"latency":          metrics.Duration.Milliseconds(),
		"upstream":         upstreamAddress(r),
	}
}

func upstreamAddress(r *http.Request) string {
	host := apisixVarString(r, "$balancer_ip")
	if host == "" {
		return ""
	}
	port := apisixVarString(r, "$balancer_port")
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func apisixVarString(r *http.Request, key string) string {
	value := apisixctx.GetApisixVar(r, key)
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (p *Plugin) resolveLogFormatValue(
	r *http.Request,
	value string,
	originalHost string,
	originalRemoteAddr string,
) any {
	switch value {
	case "$host":
		return originalHost
	case "$remote_addr":
		return originalRemoteAddr
	case "$upstream_unresolved_host":
		return apisixctx.GetApisixVar(r, "$balancer_ip")
	default:
		return apisixlog.GetField(r, value)
	}
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, _ int) (int, error) {
	return p.sendBatch(ctx, entries, false)
}

func (p *Plugin) sendBatchFromProcessor(
	ctx context.Context,
	entries []map[string]any,
	_ int,
) (int, error) {
	return p.sendBatch(ctx, entries, true)
}

func (p *Plugin) sendBatch(
	ctx context.Context,
	entries []map[string]any,
	allowRetiredDrain bool,
) (int, error) {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if (!allowRetiredDrain && p.stopped.Load()) || !p.ready || p.client == nil {
		return 0, secret.ErrCredentialUnavailable
	}
	body, err := p.encodeBatch(entries)
	if err != nil {
		return 0, err
	}
	defer clear(body)

	request := p.client.R().SetContext(ctx).SetBody(body)
	var resp *resty.Response
	defer func() {
		request.Header.Del("Authorization")
		request.Body = nil
		if request.RawRequest != nil {
			request.RawRequest.Header.Del("Authorization")
			request.RawRequest.Body = http.NoBody
			request.RawRequest.GetBody = nil
		}
		if resp != nil {
			if responseBody := resp.Body(); len(responseBody) > 0 {
				clear(responseBody)
				resp.SetBody(nil)
			}
			if resp.RawResponse != nil {
				resp.RawResponse.Header.Del("Authorization")
				resp.RawResponse.Request = nil
				resp.RawResponse.Body = http.NoBody
			}
			resp.Request = nil
			resp.RawResponse = nil
		}
	}()
	err = p.useTokenLocked(allowRetiredDrain, func(token string) error {
		request.SetHeader("Authorization", "Splunk "+token)
		var sendErr error
		resp, sendErr = request.Post(p.config.Endpoint.URI)
		return sendErr
	})
	if err != nil {
		return 0, fmt.Errorf("failed to send log to Splunk HEC endpoint %s: %w", p.config.Endpoint.URI, err)
	}

	if resp.StatusCode() != 200 {
		message := resp.String()
		var errorBody struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(resp.Body(), &errorBody); err == nil && errorBody.Text != "" {
			message = errorBody.Text
		}
		return 0, fmt.Errorf("splunk HEC endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(), p.config.Endpoint.URI, message)
	}
	return 0, nil
}

func (p *Plugin) useTokenLocked(allowRetiredDrain bool, use func(string) error) error {
	if use == nil || (!allowRetiredDrain && p.stopped.Load()) || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.tokenSet {
		return p.token.Use(use)
	}
	if p.legacyToken == nil {
		return secret.ErrCredentialUnavailable
	}
	plaintext := p.legacyToken.Bytes()
	if len(plaintext) == 0 {
		return secret.ErrCredentialUnavailable
	}
	defer clear(plaintext)
	return use(string(plaintext))
}

func (p *Plugin) encodeBatch(entries []map[string]any) ([]byte, error) {
	var body bytes.Buffer
	for _, entry := range entries {
		event, err := json.Marshal(p.buildEvent(entry))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal Splunk HEC event: %w", err)
		}
		body.Write(event)
		body.WriteByte('\n')
	}
	return body.Bytes(), nil
}

func (p *Plugin) buildEvent(log map[string]any) splunkEvent {
	hostname := base.Hostname()
	if hostname == "" {
		hostname = "-"
	}

	return splunkEvent{
		Time:       float64(time.Now().UnixNano()) / float64(time.Second),
		Host:       hostname,
		Source:     defaultSource,
		SourceType: defaultSourceType,
		Event:      log,
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}
