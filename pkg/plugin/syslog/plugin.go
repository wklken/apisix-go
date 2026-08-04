package syslog

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

const (
	priority       = 401
	name           = "syslog"
	version        = "apisix-go"
	maxFormatDepth = 5
	syslogFrameKey = "__apisix_syslog_frame"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "host": {
		"type": "string"
	  },
	  "port": {
		"type": "integer"
	  },
	  "flush_limit": {
		"type": "integer",
		"minimum": 1,
		"default": 4096
	  },
	  "drop_limit": {
		"type": "integer",
		"default": 1048576
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 3000
	  },
	  "sock_type": {
		"type": "string",
		"default": "tcp",
		"enum": ["tcp", "udp"]
	  },
	  "pool_size": {
		"type": "integer",
		"minimum": 5,
		"default": 5
	  },
	  "tls": {
		"type": "boolean",
		"default": false
	  },
	  "log_format": {
		"type": "object"
	  },
	  "log_format_extra": {
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
	"required": ["host", "port"]
}`

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
}`

type pluginMetadata struct {
	LogFormat         map[string]any `json:"log_format"`
	LogFormatExtra    map[string]any `json:"log_format_extra"`
	MaxPendingEntries int            `json:"max_pending_entries,omitempty"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config          Config
	logFormat       map[string]any
	logFormatExtra  map[string]any
	customLogFormat bool
	transport       *syslogTransport
}

type Config struct {
	Host                string         `json:"host"`
	Port                int            `json:"port"`
	FlushLimit          int            `json:"flush_limit,omitempty"`
	DropLimit           int            `json:"drop_limit,omitempty"`
	Timeout             int            `json:"timeout,omitempty"`
	LogFormat           map[string]any `json:"log_format,omitempty"`
	LogFormatExtra      map[string]any `json:"log_format_extra,omitempty"`
	SockType            string         `json:"sock_type,omitempty"`
	PoolSize            int            `json:"pool_size,omitempty"`
	TLS                 bool           `json:"tls,omitempty"`
	IncludeReqBody      bool           `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any        `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool           `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any        `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int            `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int            `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`

	addr              string
	retryDelaySet     bool
	logFormatSet      bool
	logFormatExtraSet bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type config Config

	var parsed config
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*c = Config(parsed)
	_, c.retryDelaySet = fields["retry_delay"]
	_, c.logFormatSet = fields["log_format"]
	_, c.logFormatExtraSet = fields["log_format_extra"]
	return nil
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
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.FlushLimit == 0 {
		p.config.FlushLimit = 4096
	}
	if p.config.DropLimit == 0 {
		p.config.DropLimit = 1048576
	}
	if p.config.PoolSize == 0 {
		p.config.PoolSize = 5
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

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	p.logFormat, p.logFormatExtra = selectLogFormats(p.config, metadata)
	p.customLogFormat = p.config.logFormatSet || len(p.config.LogFormat) > 0 ||
		len(metadata.LogFormat) > 0
	var truncated bool
	p.logFormat, truncated = truncateSyslogLogFormat(p.logFormat, 0)
	var extraTruncated bool
	p.logFormatExtra, extraTruncated = truncateSyslogLogFormat(p.logFormatExtra, 0)
	if truncated || extraTruncated {
		logger.Warn("log_format nesting exceeds max depth 5, truncating")
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	if p.config.SockType == "" {
		p.config.SockType = "tcp"
	}

	p.config.addr = net.JoinHostPort(p.config.Host, fmt.Sprint(p.config.Port))
	transport, err := newSyslogTransport(p.config)
	if err != nil {
		return err
	}
	p.transport = transport

	p.BatchProcessor = base.NewBatchProcessor("sys logger", base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		RetryDelaySet:      p.config.retryDelaySet,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)

	return nil
}

func (p *Plugin) Stop() {
	p.BaseLoggerPlugin.Stop()
	if p.transport != nil {
		p.transport.Close()
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		request := captureAccessRequest(r, started, p.ServerAddr)
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

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		var logFields map[string]any
		if p.customLogFormat {
			logFields = resolveSyslogLogFormat(r, request, p.logFormat)
			logFields["route_id"] = p.RouteID
			if serviceID := apisixString(r, "$service_id"); serviceID != "" {
				logFields["service_id"] = serviceID
			} else {
				delete(logFields, "service_id")
			}
		} else {
			logFields = p.defaultAccessLog(r, request, metrics, w.Header())
			for key, value := range resolveSyslogLogFormat(r, request, p.logFormatExtra) {
				if _, exists := logFields[key]; !exists {
					logFields[key] = value
				}
			}
		}
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, metrics.Code) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}

		message, err := json.Marshal(logFields)
		if err != nil {
			logger.Errorf("failed to marshal log message: %s in syslog", err)
			return
		}
		frame := encodeRFC5424(time.Now(), requestHostname(r), os.Getpid(), message)
		_ = p.Fire(map[string]any{syslogFrameKey: frame})
	}
	return http.HandlerFunc(fn)
}

func selectLogFormats(config Config, metadata pluginMetadata) (map[string]any, map[string]any) {
	if config.logFormatSet || len(config.LogFormat) > 0 {
		return config.LogFormat, nil
	}
	if len(metadata.LogFormat) > 0 {
		return metadata.LogFormat, nil
	}
	if config.logFormatExtraSet || len(config.LogFormatExtra) > 0 {
		return nil, config.LogFormatExtra
	}
	return nil, metadata.LogFormatExtra
}

type accessRequest struct {
	method        string
	uri           string
	url           string
	host          string
	clientIP      string
	contentLength int64
	headers       map[string]any
	queryString   map[string]any
	started       time.Time
}

func captureAccessRequest(r *http.Request, started time.Time, serverAddr string) accessRequest {
	headers := collapseHeaderValues(r.Header)
	headers["host"] = r.Host
	return accessRequest{
		method:        r.Method,
		uri:           r.URL.RequestURI(),
		url:           requestURL(r, serverAddr),
		host:          requestHostname(r),
		clientIP:      hostWithoutPort(r.RemoteAddr),
		contentLength: max(r.ContentLength, 0),
		headers:       headers,
		queryString:   collapseValues(r.URL.Query()),
		started:       started,
	}
}

func resolveSyslogLogFormat(
	r *http.Request,
	request accessRequest,
	format map[string]any,
) map[string]any {
	fields := make(map[string]any, len(format))
	for key, value := range format {
		fields[key] = resolveSyslogLogFormatNode(r, request, value)
	}
	return fields
}

func resolveSyslogLogFormatNode(r *http.Request, request accessRequest, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return resolveSyslogLogFormat(r, request, typed)
	case string:
		switch typed {
		case "$host":
			return request.host
		case "$remote_addr":
			return request.clientIP
		case "$time_iso8601":
			return request.started.Format(time.RFC3339)
		case "$upstream_addr":
			return upstreamAddress(r)
		default:
			return apisixlog.GetField(r, typed)
		}
	default:
		return typed
	}
}

func truncateSyslogLogFormat(format map[string]any, depth int) (map[string]any, bool) {
	result := make(map[string]any, len(format))
	truncated := false
	for key, value := range format {
		nested, ok := value.(map[string]any)
		if !ok {
			result[key] = value
			continue
		}
		if depth+1 >= maxFormatDepth {
			result[key] = map[string]any{}
			truncated = truncated || len(nested) > 0
			continue
		}
		resolved, childTruncated := truncateSyslogLogFormat(nested, depth+1)
		result[key] = resolved
		truncated = truncated || childTruncated
	}
	return result, truncated
}

func (p *Plugin) defaultAccessLog(
	r *http.Request,
	request accessRequest,
	metrics httpsnoop.Metrics,
	responseHeaders http.Header,
) map[string]any {
	hostname, _ := os.Hostname()
	latency := float64(metrics.Duration) / float64(time.Millisecond)
	upstreamLatency := requestInt64(r, "$upstream_latency")
	apisixLatency := latency - float64(upstreamLatency)
	if apisixLatency < 0 {
		apisixLatency = 0
	}
	log := map[string]any{
		"request": map[string]any{
			"url":         request.url,
			"uri":         request.uri,
			"method":      request.method,
			"headers":     request.headers,
			"querystring": request.queryString,
			"size":        request.contentLength,
		},
		"response": map[string]any{
			"status":  metrics.Code,
			"headers": collapseHeaderValues(responseHeaders),
			"size":    metrics.Written,
		},
		"server": map[string]any{
			"hostname": hostname,
			"version":  version,
		},
		"service_id":       apisixString(r, "$service_id"),
		"route_id":         p.RouteID,
		"client_ip":        request.clientIP,
		"start_time":       float64(request.started.UnixNano()) / float64(time.Millisecond),
		"latency":          latency,
		"upstream_latency": upstreamLatency,
		"apisix_latency":   apisixLatency,
		"upstream":         upstreamAddress(r),
	}
	if consumer := apisixString(r, "$consumer_name"); consumer != "" {
		log["consumer"] = map[string]any{"username": consumer}
	}
	return log
}

func requestURL(r *http.Request, serverAddr string) string {
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := requestHostname(r)
	_, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		_, port, _ = net.SplitHostPort(r.Host)
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return scheme + "://" + authority + r.URL.RequestURI()
}

func collapseHeaderValues(values http.Header) map[string]any {
	normalized := make(map[string][]string, len(values))
	for key, value := range values {
		key = strings.ToLower(key)
		normalized[key] = append(normalized[key], value...)
	}
	return collapseValues(normalized)
}

func collapseValues(values map[string][]string) map[string]any {
	collapsed := make(map[string]any, len(values))
	for key, value := range values {
		if len(value) == 1 {
			collapsed[key] = value[0]
		} else {
			collapsed[key] = value
		}
	}
	return collapsed
}

func hostWithoutPort(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return strings.Trim(address, "[]")
}

func upstreamAddress(r *http.Request) string {
	host, _ := apisixctx.GetApisixVar(r, "$balancer_ip").(string)
	port, _ := apisixctx.GetApisixVar(r, "$balancer_port").(string)
	if host == "" || port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

func apisixString(r *http.Request, key string) string {
	value, _ := apisixctx.GetApisixVar(r, key).(string)
	return value
}

func requestInt64(r *http.Request, key string) int64 {
	switch value := apisixctx.GetRequestVar(r, key).(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	default:
		return 0
	}
}

func (p *Plugin) Send(log map[string]any) {
	message, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in syslog", err)
		return
	}
	logMessage := encodeRFC5424(time.Now(), "", os.Getpid(), message)

	if err := p.sendBody(logMessage); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, batchMaxSize int) (int, error) {
	body, err := encodeBatch(entries, batchMaxSize)
	if err != nil {
		return 0, err
	}
	return 0, p.sendBody(body)
}

func encodeBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	_ = batchMaxSize
	frames := make([][]byte, 0, len(entries))
	for i, entry := range entries {
		frame, ok := entry[syslogFrameKey].([]byte)
		if !ok {
			return nil, fmt.Errorf("syslog batch entry %d does not contain an RFC5424 frame", i+1)
		}
		frames = append(frames, frame)
	}
	return joinRFC5424Frames(frames), nil
}

func encodeRFC5424(timestamp time.Time, hostname string, pid int, message []byte) []byte {
	if hostname == "" {
		hostname = "-"
	}
	header := fmt.Sprintf(
		"<46>1 %s %s apisix %d - - ",
		timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
		hostname,
		pid,
	)
	frame := make([]byte, 0, len(header)+len(message)+1)
	frame = append(frame, header...)
	frame = append(frame, message...)
	return append(frame, '\n')
}

func joinRFC5424Frames(frames [][]byte) []byte {
	size := 0
	for _, frame := range frames {
		size += len(frame)
	}
	joined := make([]byte, 0, size)
	for _, frame := range frames {
		joined = append(joined, frame...)
	}
	return joined
}

func requestHostname(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		return host
	}
	return strings.Trim(r.Host, "[]")
}

func (p *Plugin) sendBody(body []byte) error {
	if p.transport == nil {
		return errors.New("syslog transport is not initialized")
	}
	accepted, err := p.transport.Log(body)
	if accepted == len(body) {
		if err != nil {
			logger.Errorf("failed to flush accepted syslog message: %s", err)
		}
		return nil
	}
	return err
}
