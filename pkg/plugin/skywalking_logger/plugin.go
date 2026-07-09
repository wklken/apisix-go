package skywalking_logger

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
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

	metadata := loadMetadata()
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
		if p.config.IncludeReqBody && exprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *skyWalkingResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &skyWalkingResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.status
		}

		logFields := log.GetFields(r, p.LogFormat)
		if requestBody != "" {
			nestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.body.Len() > 0 && exprMatched(r, p.config.IncludeRespBodyExpr, status) {
			nestedLogMap(logFields, "response")["body"] = recorder.body.String()
		}
		logFields[internalSkyWalkingEndpoint] = r.URL.Path
		if trace, ok := parseTraceContext(r.Header.Get("sw8")); ok {
			logFields[internalSkyWalkingTraceContext] = trace
		}
		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

type skyWalkingResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *skyWalkingResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *skyWalkingResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *skyWalkingResponseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
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

func nestedLogMap(fields map[string]any, key string) map[string]any {
	if value, ok := fields[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	fields[key] = value
	return value
}

func exprMatched(r *http.Request, exprs [][]any, status int) bool {
	if len(exprs) == 0 {
		return true
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range exprs {
		if len(condition) == 1 {
			if op, ok := condition[0].(string); ok {
				switch strings.ToUpper(op) {
				case "AND", "OR":
					pendingOp = strings.ToUpper(op)
				default:
					return false
				}
				continue
			}
		}

		matched := matchCondition(r, condition, status)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}

		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func matchCondition(r *http.Request, condition []any, status int) bool {
	if len(condition) != 3 {
		return false
	}

	left := fmt.Sprint(condition[0])
	op := fmt.Sprint(condition[1])
	right := fmt.Sprint(condition[2])
	actual := requestVar(r, left, status)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

func requestVar(r *http.Request, name string, status int) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code":
		if status > 0 {
			return strconv.Itoa(status)
		}
		return fmt.Sprint(apisixctx.GetRequestVar(r, "$status"))
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
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

func parseTraceContext(header string) (*traceContext, bool) {
	if header == "" {
		return nil, false
	}
	parts := strings.Split(header, "-")
	if len(parts) != 8 {
		return nil, false
	}

	traceID, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil, false
	}
	segmentID, err := decodeBase64URL(parts[2])
	if err != nil {
		return nil, false
	}
	spanID, err := strconv.Atoi(parts[3])
	if err != nil {
		return nil, false
	}

	return &traceContext{
		TraceID:        traceID,
		TraceSegmentID: segmentID,
		SpanID:         spanID,
	}, true
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

func loadMetadata() (metadata pluginMetadata) {
	defer func() {
		if recover() != nil {
			metadata = pluginMetadata{}
		}
	}()

	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return pluginMetadata{}
	}
	return metadata
}
