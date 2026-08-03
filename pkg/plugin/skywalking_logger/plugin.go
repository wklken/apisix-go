package skywalking_logger

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

const (
	priority = 408
	name     = "skywalking-logger"

	internalSkyWalkingEndpoint     = "_skywalking_endpoint"
	internalSkyWalkingTraceContext = "_skywalking_trace_context"
)

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addr": {
      "type": "string",
      "format": "uri"
    },
    "service_name": {
      "type": "string",
      "default": "APISIX"
    },
    "service_instance_name": {
      "type": "string",
      "default": "APISIX Instance Name"
    },
    "log_format": {
      "type": "object"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
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
  "required": ["endpoint_addr"]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Config struct {
	EndpointAddr        string            `json:"endpoint_addr"`
	ServiceName         string            `json:"service_name,omitempty"`
	ServiceInstanceName string            `json:"service_instance_name,omitempty"`
	LogFormat           map[string]string `json:"log_format,omitempty"`
	Timeout             int               `json:"timeout,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any           `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any           `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

type skyWalkingEntry struct {
	TraceContext    *traceContext `json:"traceContext,omitempty"`
	Body            logBody       `json:"body"`
	Service         string        `json:"service"`
	ServiceInstance string        `json:"serviceInstance"`
	Endpoint        string        `json:"endpoint"`
}

type traceContext struct {
	TraceID        string `json:"traceId"`
	TraceSegmentID string `json:"traceSegmentId"`
	SpanID         int    `json:"spanId"`
}

type logBody struct {
	JSON jsonWrapper `json:"json"`
}

type jsonWrapper struct {
	JSON string `json:"json"`
}

func (p *Plugin) Config() any {
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
	if p.config.ServiceName == "" {
		p.config.ServiceName = "APISIX"
	}
	if p.config.ServiceInstanceName == "" {
		p.config.ServiceInstanceName = "APISIX Instance Name"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
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
	configUID.Add(p.config.EndpointAddr)
	configUID.Add(p.config.Timeout)

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	client.SetHeader("Content-Type", "application/json")
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "skywalking logger",
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

		logFields := p.logFields(r, status)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}
		logFields[internalSkyWalkingEndpoint] = r.URL.Path
		if sw8 := r.Header.Get("sw8"); sw8 != "" {
			trace, err := parseTraceContext(sw8)
			if err != nil {
				logger.Warnf("failed to parse trace_context header: %s: %v", sw8, err)
			} else {
				logFields[internalSkyWalkingTraceContext] = trace
			}
		}
		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) logFields(r *http.Request, status int) map[string]any {
	fields := make(map[string]any, len(p.LogFormat)+2)
	for key, value := range p.LogFormat {
		switch value {
		case "$host", "$remote_addr":
			fields[key] = base.RequestVar(r, value, status)
		default:
			fields[key] = log.GetField(r, value)
		}
	}
	if routeID := base.RequestVar(r, "$route_id", status); routeID != "" {
		fields["route_id"] = routeID
	}
	if serviceID := base.RequestVar(r, "$service_id", status); serviceID != "" {
		fields["service_id"] = serviceID
	}
	return fields
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, batchMaxSize int) (int, error) {
	_ = batchMaxSize

	endpoint := p.endpointURL()
	resp, err := p.client.R().SetBody(p.buildEntries(entries)).Post(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to send log to SkyWalking endpoint %s: %w", endpoint, err)
	}

	if resp.StatusCode() >= 400 {
		return 0, fmt.Errorf(
			"SkyWalking endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}

	return 0, nil
}

func (p *Plugin) buildEntries(logs []map[string]any) []skyWalkingEntry {
	entries := make([]skyWalkingEntry, 0, len(logs))
	for _, logEntry := range logs {
		entries = append(entries, p.buildEntry(logEntry))
	}
	return entries
}

func (p *Plugin) buildEntry(log map[string]any) skyWalkingEntry {
	payload := make(map[string]any, len(log))
	for key, value := range log {
		if key == internalSkyWalkingEndpoint || key == internalSkyWalkingTraceContext {
			continue
		}
		payload[key] = value
	}

	body, err := json.Marshal(payload)
	if err != nil {
		body = []byte(`{}`)
	}

	entry := skyWalkingEntry{
		Body: logBody{
			JSON: jsonWrapper{
				JSON: string(body),
			},
		},
		Service:         p.config.ServiceName,
		ServiceInstance: p.serviceInstanceName(),
		Endpoint:        endpointFromLog(log),
	}
	if trace, ok := log[internalSkyWalkingTraceContext].(*traceContext); ok {
		entry.TraceContext = trace
	}
	return entry
}

func endpointFromLog(log map[string]any) string {
	if endpoint, ok := log[internalSkyWalkingEndpoint].(string); ok {
		return endpoint
	}
	return ""
}

func (p *Plugin) serviceInstanceName() string {
	if p.config.ServiceInstanceName != "$hostname" {
		return p.config.ServiceInstanceName
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return "$hostname"
	}
	return hostname
}

func (p *Plugin) endpointURL() string {
	return strings.TrimRight(p.config.EndpointAddr, "/") + "/v3/logs"
}

func parseTraceContext(header string) (*traceContext, error) {
	if header == "" {
		return nil, fmt.Errorf("header is empty")
	}
	parts := strings.Split(header, "-")
	if len(parts) != 8 {
		return nil, fmt.Errorf("got %d parts, want 8", len(parts))
	}

	traceID, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode trace ID: %w", err)
	}
	segmentID, err := decodeBase64URL(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode trace segment ID: %w", err)
	}
	spanID, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, fmt.Errorf("decode span ID: %w", err)
	}

	return &traceContext{
		TraceID:        traceID,
		TraceSegmentID: segmentID,
		SpanID:         spanID,
	}, nil
}

func decodeBase64URL(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return string(decoded), nil
	}
	decoded, err = base64.URLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
