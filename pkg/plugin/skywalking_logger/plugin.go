package skywalking_logger

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
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

	clientRelease func()
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
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
	p.MetadataSchema = metadataSchema

	p.InitLogger(p.Send)

	return nil
}

func (p *Plugin) PostInit() error {
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("skywalking-logger metadata decode failed: %w", err)
	}
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
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	p.client = value.(*resty.Client)
	p.clientRelease = release

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

	processor, err := base.NewBatchProcessor("skywalking logger", p.TaskOwner(), base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)
	if err != nil {
		return err
	}
	p.BatchProcessor = processor

	return nil
}

func (p *Plugin) QuiesceGenerationTasks() { p.Stop() }

func (p *Plugin) Stop() {
	p.StopWithCleanup(func() {
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
	})
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
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
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

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	fields := base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	if routeID := fmt.Sprint(base.SnapshotValue(snapshot, "$route_id")); routeID != "" {
		fields["route_id"] = routeID
	}
	if serviceID := fmt.Sprint(base.SnapshotValue(snapshot, "$service_id")); serviceID != "" {
		fields["service_id"] = serviceID
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
	fields[internalSkyWalkingEndpoint] = snapshotPath(snapshot)
	if sw8 := snapshot.Request.Header.Get("sw8"); sw8 != "" {
		trace, err := parseTraceContext(sw8)
		if err != nil {
			logger.Warnf("failed to parse trace_context header: %s: %v", sw8, err)
		} else {
			fields[internalSkyWalkingTraceContext] = trace
		}
	}
	return p.EnqueueLog(fields)
}

func snapshotPath(snapshot base.LogSnapshot) string {
	if parsed, err := url.ParseRequestURI(snapshot.Request.URI); err == nil {
		return parsed.Path
	}
	return snapshot.Request.URI
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
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	_ = batchMaxSize

	endpoint := p.endpointURL()
	entriesPayload, err := p.buildEntries(entries)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.R().SetContext(ctx).SetBody(entriesPayload).Post(endpoint)
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

func (p *Plugin) buildEntries(logs []map[string]any) ([]skyWalkingEntry, error) {
	entries := make([]skyWalkingEntry, 0, len(logs))
	for _, logEntry := range logs {
		entry, err := p.buildEntry(logEntry)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (p *Plugin) buildEntry(log map[string]any) (skyWalkingEntry, error) {
	payload := make(map[string]any, len(log))
	for key, value := range log {
		if key == internalSkyWalkingEndpoint || key == internalSkyWalkingTraceContext {
			continue
		}
		payload[key] = value
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return skyWalkingEntry{}, fmt.Errorf("failed to marshal skywalking log entry: %w", err)
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
	return entry, nil
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
	if hostname := base.Hostname(); hostname != "" {
		return hostname
	}
	return "$hostname"
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
