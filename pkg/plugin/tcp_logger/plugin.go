package tcp_logger

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

const (
	priority = 405
	name     = "tcp-logger"
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
	  "tls": {
		"type": "boolean",
		"default": false
	  },
	  "tls_options": {
		"type": "string"
	  },
	  "timeout": {
		"type": "integer",
		"minimum": 1,
		"default": 1000
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

	logFormat map[string]any

	// connMu serializes sends so batches share one connection and never
	// interleave on the stream. The connection is closed and re-dialed on any
	// write failure.
	connMu sync.Mutex
	conn   net.Conn
}

type Config struct {
	Host                string         `json:"host"`
	Port                int            `json:"port"`
	TLS                 bool           `json:"tls,omitempty"`
	Timeout             int            `json:"timeout,omitempty"`
	TLSOptions          *string        `json:"tls_options,omitempty"`
	LogFormat           map[string]any `json:"log_format,omitempty"`
	IncludeReqBody      bool           `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any          `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool           `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any          `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int            `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int            `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`

	addr          string
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

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	p.InitLogger(p.Send)

	return nil
}

func (p *Plugin) PostInit() error {
	base.PrepareExprRegexps(p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr)
	if p.config.Timeout == 0 {
		p.config.Timeout = 1000
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

	p.config.addr = net.JoinHostPort(p.config.Host, fmt.Sprint(p.config.Port))

	p.BatchProcessor = base.NewBatchProcessor("tcp logger", base.BatchDefaults{
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

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		request := base.CaptureMinimalAccessLogRequest(r, started)
		if len(p.logFormat) == 0 {
			request = base.CaptureAccessLogRequest(r, started, p.ServerAddr)
		}

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

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		var logFields map[string]any
		if len(p.logFormat) > 0 {
			logFields = resolveTCPLogFormat(r, request, p.logFormat)
			logFields["route_id"] = p.RouteID
			if serviceID := base.ApisixString(r, "$service_id"); serviceID != "" {
				logFields["service_id"] = serviceID
			} else {
				delete(logFields, "service_id")
			}
		} else {
			logFields = base.BuildAccessLogSnapshot(
				request,
				metrics.Code,
				w.Header(),
				metrics.Written,
				p.RouteID,
				r,
				metrics.Duration,
			)
		}
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, metrics.Code) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

type accessRequest = base.AccessLogRequest

func resolveTCPLogFormat(r *http.Request, request accessRequest, format map[string]any) map[string]any {
	return base.ResolveLogFormat(format, func(value string) any {
		switch value {
		case "$host":
			return request.Host
		case "$remote_addr":
			return request.ClientIP
		case "$time_iso8601":
			return request.Started.Format(time.RFC3339)
		default:
			return apisixlog.GetField(r, value)
		}
	})
}

func (p *Plugin) Send(log map[string]any) {
	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in tcp-logger", err)
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
	body, err := base.EncodeLogBatch(entries, batchMaxSize, "")
	if err != nil {
		entryLabel := "entries"
		if batchMaxSize == 1 && len(entries) == 1 {
			entryLabel = "entry"
		}
		return nil, fmt.Errorf("failed to marshal tcp log %s: %w", entryLabel, err)
	}
	return body, nil
}

func (p *Plugin) sendBody(body []byte) error {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	conn := p.conn
	if conn == nil {
		var err error
		conn, err = p.dial()
		if err != nil {
			return fmt.Errorf(
				"failed to connect to TCP server: host[%s] port[%d]: %w",
				p.config.Host,
				p.config.Port,
				err,
			)
		}
		p.conn = conn
	}

	deadline := time.Now().Add(time.Duration(p.config.Timeout) * time.Millisecond)
	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline)

	written := 0
	for written < len(body) {
		n, err := conn.Write(body[written:])
		written += n
		if err != nil {
			_ = conn.Close()
			p.conn = nil
			return fmt.Errorf("failed to send log message: %s in tcp-logger", err)
		}
	}
	return nil
}

// Stop drains the batch processor first, then closes the shared connection.
func (p *Plugin) Stop() {
	p.BaseLoggerPlugin.Stop()

	p.connMu.Lock()
	if p.conn != nil {
		_ = p.conn.Close()
		p.conn = nil
	}
	p.connMu.Unlock()
}

func (p *Plugin) dial() (net.Conn, error) {
	dialer := &net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	if !p.config.TLS {
		return dialer.Dial("tcp", p.config.addr)
	}

	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	if p.config.TLSOptions != nil {
		tlsConfig.ServerName = *p.config.TLSOptions
	}
	return tls.DialWithDialer(dialer, "tcp", p.config.addr, tlsConfig)
}
