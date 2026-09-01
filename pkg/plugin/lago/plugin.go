package lago

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
	now    func() time.Time

	clientRelease func()
	lifecycleMu   sync.RWMutex
	stopped       atomic.Bool

	token           secret.Value
	tokenSet        bool
	secretsPrepared bool
	ready           bool
}

const (
	priority = 415
	name     = "lago"

	defaultBatchMaxSize = 100

	requestStartTimeField = "__lago_request_start_time"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "endpoint_uri": {
      "type": "string",
      "minLength": 1,
      "default": "/api/v1/events/batch"
    },
    "token": {
      "type": "string"
    },
    "event_transaction_id": {
      "type": "string"
    },
    "event_subscription_id": {
      "type": "string"
    },
    "event_code": {
      "type": "string"
    },
    "event_properties": {
      "type": "object",
      "additionalProperties": {
        "type": "string",
        "minLength": 1
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "include_req_body": {
      "type": "boolean",
      "default": false
    },
    "include_resp_body": {
      "type": "boolean",
      "default": false
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
      "default": 100
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
    }
  },
  "required": ["endpoint_addrs", "token", "event_transaction_id", "event_subscription_id", "event_code"]
}
`

type Config struct {
	EndpointAddrs       []string          `json:"endpoint_addrs"`
	EndpointURI         string            `json:"endpoint_uri,omitempty"`
	Token               string            `json:"token"`
	EventTransactionID  string            `json:"event_transaction_id"`
	EventSubscriptionID string            `json:"event_subscription_id"`
	EventCode           string            `json:"event_code"`
	EventProperties     map[string]string `json:"event_properties,omitempty"`
	SSLVerify           *bool             `json:"ssl_verify,omitempty"`
	Timeout             int               `json:"timeout,omitempty"`
	Keepalive           *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout    int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool       int               `json:"keepalive_pool,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize    int `json:"batch_max_size,omitempty"`
	InactiveTimeout int `json:"inactive_timeout,omitempty"`
	BufferDuration  int `json:"buffer_duration,omitempty"`
	RetryDelay      int `json:"retry_delay,omitempty"`
	MaxRetryCount   int `json:"max_retry_count,omitempty"`
}

type lagoPayload struct {
	Events []lagoEvent `json:"events"`
}

type lagoEvent struct {
	TransactionID          string            `json:"transaction_id"`
	ExternalSubscriptionID string            `json:"external_subscription_id"`
	Code                   string            `json:"code"`
	Timestamp              float64           `json:"timestamp"`
	Properties             map[string]string `json:"properties,omitempty"`
}

var templatePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

var randomEndpointIndex = rand.Intn

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

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
	value, err := access.Materialize(ctx, "token", p.config.Token)
	if err != nil || value.Use(validateLagoToken) != nil {
		return lagoTokenUnavailable()
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return lagoTokenUnavailable()
	}
	p.token = value
	p.tokenSet = true
	p.config.Token = descriptor.String()
	p.secretsPrepared = true
	return nil
}

func validateLagoToken(value string) error {
	if strings.TrimSpace(value) == "" {
		return secret.ErrCredentialUnavailable
	}
	return nil
}

func lagoTokenUnavailable() error {
	return fmt.Errorf("%s token: %w", name, secret.ErrCredentialUnavailable)
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
	if p.config.EndpointURI == "" {
		p.config.EndpointURI = "/api/v1/events/batch"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.Keepalive == nil {
		value := true
		p.config.Keepalive = &value
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		nil, nil,
	)
	if p.config.SSLVerify == nil {
		value := true
		p.config.SSLVerify = &value
	}
	if p.config.BatchMaxSize == 0 {
		p.config.BatchMaxSize = defaultBatchMaxSize
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
	if p.now == nil {
		p.now = time.Now
	}

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddrs)
	configUID.Add(p.config.EndpointURI)
	configUID.Add(p.config.Timeout)
	configUID.Add(*p.config.SSLVerify)
	configUID.Add(p.keepalive())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !*p.config.SSLVerify})
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	sharedClient := value.(*resty.Client)
	processor, err := base.NewBatchProcessor("lago logger", p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
	}, p.RouteID, p.ServerAddr, p.sendBatchFromProcessor)
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
	p.ready = true
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

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	return p.enqueueLagoLogIfRunning(p.lagoSnapshotFields(snapshot))
}

func (p *Plugin) enqueueLagoLogIfRunning(fields map[string]any) error {
	p.lifecycleMu.RLock()
	defer p.lifecycleMu.RUnlock()
	if p.stopped.Load() || !p.ready || p.BatchProcessor == nil {
		return base.ErrLogQueueUnavailable
	}
	return p.EnqueueLog(fields)
}

func (p *Plugin) lagoSnapshotFields(snapshot base.LogSnapshot) map[string]any {
	fields := map[string]any{
		"status":              snapshot.Outcome.Status,
		requestStartTimeField: snapshot.Started,
	}
	if p.config.IncludeReqBody {
		fields["request_body"] = base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes)
	}
	if p.config.IncludeRespBody {
		fields["response_body"] = base.SnapshotResponseBody(snapshot, p.config.MaxRespBodyBytes)
	}
	for _, template := range p.templates() {
		for _, name := range templateVariables(template) {
			if _, ok := fields[name]; ok {
				continue
			}
			fields[name] = lagoSnapshotVariable(snapshot, name)
		}
	}
	return fields
}

func lagoSnapshotVariable(snapshot base.LogSnapshot, name string) any {
	if name == "status" {
		return snapshot.Outcome.Status
	}
	if after, ok := strings.CutPrefix(name, "cookie_"); ok {
		req := &http.Request{Header: snapshot.Request.Header}
		if cookie, err := req.Cookie(after); err == nil {
			return cookie.Value
		}
		return ""
	}
	if after, ok := strings.CutPrefix(name, "sent_http_"); ok {
		return snapshot.Response.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}
	if after, ok := strings.CutPrefix(name, "upstream_http_"); ok {
		return snapshot.Response.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}
	return base.SnapshotValue(snapshot, "$"+name)
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
	if len(p.config.EndpointAddrs) == 0 {
		return 0, nil
	}

	events := make([]lagoEvent, 0, len(entries))
	for _, entry := range entries {
		events = append(events, p.buildEvent(entry))
	}
	endpoint := p.endpointURL()
	request := p.client.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(lagoPayload{Events: events})
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

	err := p.useTokenLocked(allowRetiredDrain, func(token string) error {
		request.SetHeader("Authorization", "Bearer "+token)
		var err error
		resp, err = request.Post(endpoint)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("failed to send Lago event to endpoint %s: %w", endpoint, err)
	}
	if resp.StatusCode() >= 300 {
		return 0, fmt.Errorf(
			"lago endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}
	return 0, nil
}

func (p *Plugin) useTokenLocked(allowRetiredDrain bool, use func(string) error) error {
	if (!allowRetiredDrain && p.stopped.Load()) || !p.secretsPrepared {
		return secret.ErrCredentialUnavailable
	}
	if p.tokenSet {
		return p.token.Use(use)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) buildEvent(fields map[string]any) lagoEvent {
	entry := lagoEvent{
		TransactionID:          resolveTemplate(p.config.EventTransactionID, fields),
		ExternalSubscriptionID: resolveTemplate(p.config.EventSubscriptionID, fields),
		Code:                   p.config.EventCode,
		Timestamp:              p.eventTimestamp(fields),
	}

	if len(p.config.EventProperties) > 0 {
		entry.Properties = make(map[string]string, len(p.config.EventProperties))
		for key, value := range p.config.EventProperties {
			entry.Properties[key] = resolveTemplate(value, fields)
		}
	}

	return entry
}

func (p *Plugin) eventTimestamp(fields map[string]any) float64 {
	if start, ok := fields[requestStartTimeField].(time.Time); ok {
		return unixSeconds(start)
	}
	return unixSeconds(p.now())
}

func unixSeconds(value time.Time) float64 {
	return float64(value.UnixNano()) / float64(time.Second)
}

func (p *Plugin) templates() []string {
	templates := []string{p.config.EventTransactionID, p.config.EventSubscriptionID}
	for _, value := range p.config.EventProperties {
		templates = append(templates, value)
	}
	return templates
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))], "/") +
		p.config.EndpointURI
}

func (p *Plugin) keepalive() bool {
	return p.config.Keepalive == nil || *p.config.Keepalive
}

func resolveTemplate(template string, fields map[string]any) string {
	return templatePattern.ReplaceAllStringFunc(template, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(match, "${"), "}")
		if fields[name] == nil {
			return ""
		}
		return fmt.Sprint(fields[name])
	})
}

func templateVariables(template string) []string {
	matches := templatePattern.FindAllStringSubmatch(template, -1)
	variables := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) == 2 {
			variables = append(variables, match[1])
		}
	}
	return variables
}
