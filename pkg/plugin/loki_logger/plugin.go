package loki_logger

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client         *resty.Client
	logFormatExtra map[string]string
}

const (
	priority = 414
	name     = "loki-logger"
)

var randomEndpointIndex = rand.Intn

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
      "default": "/loki/api/v1/push"
    },
    "tenant_id": {
      "type": "string",
      "default": "fake"
    },
    "headers": {
      "type": "object"
    },
    "log_labels": {
      "type": "object",
      "default": {
        "job": "apisix"
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
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
      "minimum": 1
    }
  },
  "required": ["endpoint_addrs"]
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

type Config struct {
	EndpointAddrs    []string          `json:"endpoint_addrs"`
	EndpointURI      string            `json:"endpoint_uri,omitempty"`
	TenantID         string            `json:"tenant_id,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	LogLabels        map[string]string `json:"log_labels,omitempty"`
	SSLVerify        bool              `json:"ssl_verify"`
	Timeout          int               `json:"timeout,omitempty"`
	Keepalive        *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int               `json:"keepalive_pool,omitempty"`
	LogFormat        map[string]string `json:"log_format,omitempty"`

	IncludeReqBody     bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr [][]any `json:"include_req_body_expr,omitempty"`

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

type lokiPayload struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	p.FireChan = make(chan map[string]any, 1000)
	p.AsyncBlock = true
	p.SendFunc = p.Send

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.EndpointURI == "" {
		p.config.EndpointURI = "/loki/api/v1/push"
	}
	if p.config.TenantID == "" {
		p.config.TenantID = "fake"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if len(p.config.LogLabels) == 0 {
		p.config.LogLabels = map[string]string{"job": "apisix"}
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

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddrs)
	configUID.Add(p.config.EndpointURI)
	configUID.Add(p.config.TenantID)
	configUID.Add(p.config.Headers)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.config.SSLVerify)
	configUID.Add(p.keepalive())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SSLVerify})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
		p.logFormatExtra = metadata.LogFormatExtra
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "loki logger",
		BatchMaxSize:      p.config.BatchMaxSize,
		MaxRetryCount:     p.config.MaxRetryCount,
		RetryDelay:        time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:    time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout:   time.Duration(p.config.InactiveTimeout) * time.Second,
		MaxPendingEntries: p.config.MaxPendingEntries,
		RouteID:           p.RouteID,
		ServerAddr:        p.ServerAddr,
	}, p.SendBatch)

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var requestBody string
		if p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.ResponseRecorder
		if p.config.IncludeRespBody {
			recorder = base.NewResponseRecorder(w, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := p.logFields(r)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) logFields(r *http.Request) map[string]any {
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = apisixlog.GetFields(r, p.LogFormat)
	} else {
		fields = p.defaultLogFields(r)
	}
	for key, value := range p.logFormatExtra {
		fields[key] = p.resolveLogFormatValue(r, value)
	}
	return fields
}

func (p *Plugin) defaultLogFields(r *http.Request) map[string]any {
	fields := map[string]any{
		"request_method": r.Method,
		"request_uri":    r.URL.RequestURI(),
		"remote_addr":    base.RequestVar(r, "$remote_addr", 0),
	}
	if routeID := base.RequestVar(r, "$route_id", 0); routeID != "" {
		fields["route_id"] = routeID
	}
	for key, values := range r.Header {
		name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
		value := strings.Join(values, ",")
		fields["request_headers_"+name] = value
		fields["http_"+name] = value
	}
	return fields
}

func (p *Plugin) resolveLogFormatValue(r *http.Request, value string) any {
	if value == "$upstream_unresolved_host" {
		return base.RequestVar(r, "$balancer_ip", 0)
	}
	return apisixlog.GetField(r, value)
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, batchMaxSize int) (int, error) {
	_ = batchMaxSize

	if len(p.config.EndpointAddrs) == 0 {
		return 0, fmt.Errorf("loki-logger endpoint_addrs is empty")
	}

	endpoint := p.endpointURL()
	resp, err := p.client.R().
		SetHeaders(p.headers()).
		SetBody(p.buildBatchPayload(entries)).
		Post(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to send log to Loki endpoint %s: %w", endpoint, err)
	}

	if resp.StatusCode() >= 300 {
		return 0, fmt.Errorf(
			"loki endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}

	return 0, nil
}

func (p *Plugin) buildPayload(log map[string]any) lokiPayload {
	return p.buildBatchPayload([]map[string]any{log})
}

func (p *Plugin) buildBatchPayload(entries []map[string]any) lokiPayload {
	streams := make([]lokiStream, 0, len(entries))
	streamIndex := make(map[string]int, len(entries))
	for _, logEntry := range entries {
		logTime := fmt.Sprintf("%d", time.Now().UnixNano())
		if value, ok := logEntry["loki_log_time"]; ok {
			logTime = fmt.Sprint(value)
		}

		entry := logEntry
		if _, ok := logEntry["loki_log_time"]; ok {
			entry = make(map[string]any, len(logEntry)-1)
			for key, value := range logEntry {
				if key != "loki_log_time" {
					entry[key] = value
				}
			}
		}

		body, err := json.Marshal(entry)
		if err != nil {
			body = []byte(`{}`)
		}
		labels := p.resolveLabels(logEntry)
		labelKey, err := json.Marshal(labels)
		if err != nil {
			labelKey = []byte{}
		}
		index, ok := streamIndex[string(labelKey)]
		if !ok {
			index = len(streams)
			streamIndex[string(labelKey)] = index
			streams = append(streams, lokiStream{Stream: labels})
		}
		streams[index].Values = append(streams[index].Values, [2]string{logTime, string(body)})
	}
	return lokiPayload{Streams: streams}
}

func (p *Plugin) resolveLabels(log map[string]any) map[string]string {
	labels := make(map[string]string, len(p.config.LogLabels))
	for key, value := range p.config.LogLabels {
		if after, ok := strings.CutPrefix(value, "$"); ok {
			if resolved, ok := log[after]; ok {
				labels[key] = fmt.Sprint(resolved)
			}
			if _, ok := labels[key]; !ok {
				labels[key] = ""
			}
			continue
		}
		labels[key] = value
	}
	return labels
}

func (p *Plugin) headers() map[string]string {
	headers := make(map[string]string, len(p.config.Headers)+2)
	for key, value := range p.config.Headers {
		if strings.EqualFold(key, "X-Scope-OrgID") || strings.EqualFold(key, "Content-Type") {
			continue
		}
		headers[key] = value
	}
	headers["X-Scope-OrgID"] = p.config.TenantID
	headers["Content-Type"] = "application/json"
	return headers
}

func (p *Plugin) endpointURL() string {
	baseURL := p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
	if strings.HasSuffix(baseURL, "/") && strings.HasPrefix(p.config.EndpointURI, "/") {
		return baseURL[:len(baseURL)-1] + p.config.EndpointURI
	}
	return baseURL + p.config.EndpointURI
}

func (p *Plugin) keepalive() bool {
	return p.config.Keepalive == nil || *p.config.Keepalive
}
