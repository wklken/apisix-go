package udp_logger

import (
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
	priority = 400
	name     = "udp-logger"
	version  = "apisix-go"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "host": {
		"type": "string"
	  },
	  "port": {
		"type": "integer",
		"minimum": 0
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
}`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
}

type Config struct {
	Host                string            `json:"host"`
	Port                int               `json:"port"`
	Timeout             int               `json:"timeout,omitempty"`    // 使用指针以区分默认值和未设置
	LogFormat           map[string]string `json:"log_format,omitempty"` // 使用指针类型以便跳过默认空值
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any             `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any             `json:"include_resp_body_expr,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`

	addr string
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

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) == 0 {
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.config.addr = net.JoinHostPort(p.config.Host, fmt.Sprint(p.config.Port))

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "udp logger",
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
		if len(p.LogFormat) > 0 {
			logFields = resolveUDPLogFormat(r, request, p.LogFormat)
		} else {
			logFields = p.defaultAccessLog(r, request, metrics, w.Header())
		}

		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, metrics.Code) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
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
	return accessRequest{
		method:        r.Method,
		uri:           r.URL.RequestURI(),
		url:           requestURL(r, serverAddr),
		host:          hostWithoutPort(r.Host),
		clientIP:      hostWithoutPort(r.RemoteAddr),
		contentLength: max(r.ContentLength, 0),
		headers:       collapseStringValues(r.Header),
		queryString:   collapseStringValues(r.URL.Query()),
		started:       started,
	}
}

func resolveUDPLogFormat(r *http.Request, request accessRequest, format map[string]string) map[string]any {
	fields := make(map[string]any, len(format))
	for key, value := range format {
		switch value {
		case "$host":
			fields[key] = request.host
		case "$remote_addr":
			fields[key] = request.clientIP
		case "$time_iso8601":
			fields[key] = request.started.Format(time.RFC3339)
		default:
			fields[key] = apisixlog.GetField(r, value)
		}
	}
	return fields
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
			"headers": collapseStringValues(responseHeaders),
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
	host := hostWithoutPort(r.Host)
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

func collapseStringValues(values map[string][]string) map[string]any {
	normalized := make(map[string][]string, len(values))
	for key, value := range values {
		key = strings.ToLower(key)
		normalized[key] = append(normalized[key], value...)
	}
	collapsed := make(map[string]any, len(normalized))
	for key, value := range normalized {
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

func requestInt(r *http.Request, key string) int {
	switch value := apisixctx.GetRequestVar(r, key).(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func requestInt64(r *http.Request, key string) int64 {
	return int64(requestInt(r, key))
}

func (p *Plugin) Send(log map[string]any) {
	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in udp-logger", err)
		return
	}

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
	if batchMaxSize == 1 && len(entries) == 1 {
		body, err := json.Marshal(entries[0])
		if err != nil {
			return nil, fmt.Errorf("failed to marshal udp log entry: %w", err)
		}
		return body, nil
	}

	body, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal udp log entries: %w", err)
	}
	return body, nil
}

func (p *Plugin) sendBody(body []byte) error {
	conn, err := p.dial()
	if err != nil {
		return fmt.Errorf(
			"failed to connect to udp server: host[%s] port[%d]: %w",
			p.config.Host,
			p.config.Port,
			err,
		)
	}
	defer func() { _ = conn.Close() }()

	if _, err = conn.Write(body); err != nil {
		return fmt.Errorf("failed to send log message: %s in udp-logger", err)
	}
	return nil
}

func (p *Plugin) dial() (net.Conn, error) {
	dialer := &net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Second}
	return dialer.Dial("udp", p.config.addr)
}
