package lago

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
	now    func() time.Time
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

type responseRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	limit  int
}

func (w *responseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *responseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
}

var templatePattern = regexp.MustCompile(`\$\{([^}]+)\}`)

var randomEndpointIndex = rand.Intn

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
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
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:            "lago logger",
		BatchMaxSize:    p.config.BatchMaxSize,
		MaxRetryCount:   p.config.MaxRetryCount,
		RetryDelay:      time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:  time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout: time.Duration(p.config.InactiveTimeout) * time.Second,
	}, p.SendBatch)
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		requestStart := p.now()

		var requestBody string
		if p.config.IncludeReqBody {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		responseLimit := 0
		if p.config.IncludeRespBody {
			responseLimit = p.config.MaxRespBodyBytes
		}
		recorder := &responseRecorder{
			ResponseWriter: w,
			limit:          responseLimit,
		}
		next.ServeHTTP(recorder, r)
		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}

		var responseBody string
		if p.config.IncludeRespBody && recorder.body.Len() > 0 {
			responseBody = recorder.body.String()
		}

		p.Fire(p.logFields(r, recorder.status, requestBody, responseBody, requestStart, recorder.Header()))
	}
	return http.HandlerFunc(fn)
}

func readAndRestoreRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return string(body), nil
}

func (p *Plugin) Send(fields map[string]any) {
	if _, err := p.SendBatch([]map[string]any{fields}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, _ int) (int, error) {
	if len(p.config.EndpointAddrs) == 0 {
		return 0, nil
	}

	events := make([]lagoEvent, 0, len(entries))
	for _, entry := range entries {
		events = append(events, p.buildEvent(entry))
	}
	endpoint := p.endpointURL()
	resp, err := p.client.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", "Bearer "+p.config.Token).
		SetBody(lagoPayload{Events: events}).
		Post(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to send Lago event to endpoint %s: %w", endpoint, err)
	}
	if resp.StatusCode() >= 300 {
		return 0, fmt.Errorf(
			"Lago endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}
	return 0, nil
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

func (p *Plugin) logFields(
	r *http.Request,
	status int,
	requestBody string,
	responseBody string,
	requestStart time.Time,
	responseHeader http.Header,
) map[string]any {
	fields := map[string]any{
		"status":              status,
		requestStartTimeField: requestStart,
	}
	if p.config.IncludeReqBody {
		fields["request_body"] = requestBody
	}
	if p.config.IncludeRespBody {
		fields["response_body"] = responseBody
	}
	for _, template := range p.templates() {
		for _, name := range templateVariables(template) {
			if _, ok := fields[name]; ok {
				continue
			}
			fields[name] = requestVariable(r, name, status, responseHeader)
		}
	}
	return fields
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

func requestVariable(r *http.Request, name string, status int, responseHeader http.Header) any {
	if name == "status" {
		return status
	}
	if strings.HasPrefix(name, "arg_") {
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	}
	if strings.HasPrefix(name, "cookie_") {
		cookie, err := r.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err != nil {
			return ""
		}
		return cookie.Value
	}
	if strings.HasPrefix(name, "http_") {
		return r.Header.Get(strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-"))
	}
	if strings.HasPrefix(name, "sent_http_") {
		return responseHeader.Get(strings.ReplaceAll(strings.TrimPrefix(name, "sent_http_"), "_", "-"))
	}
	if strings.HasPrefix(name, "upstream_http_") {
		return responseHeader.Get(strings.ReplaceAll(strings.TrimPrefix(name, "upstream_http_"), "_", "-"))
	}

	return log.GetField(r, "$"+name)
}
