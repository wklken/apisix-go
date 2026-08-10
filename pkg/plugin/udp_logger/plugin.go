package udp_logger

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

const (
	priority = 400
	name     = "udp-logger"
	version  = "apisix-go"

	// maxUDPDatagramSize is the largest IPv4 UDP payload that fits in a
	// single datagram without IP fragmentation.
	maxUDPDatagramSize = 65507
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

// Plugin implements the udp-logger plugin, delivering access log entries to
// a UDP socket through the shared batch processor.
type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	// conn is a test seam for deterministic write-failure coverage; the
	// production path always dials a fresh socket per batch.
	conn net.Conn
}

// Config carries the udp-logger plugin configuration.
type Config struct {
	Host                string            `json:"host"`
	Port                int               `json:"port"`
	Timeout             int               `json:"timeout,omitempty"`
	LogFormat           map[string]string `json:"log_format,omitempty"`
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

// Config returns the plugin configuration.
func (p *Plugin) Config() any {
	return &p.config
}

// Init registers the plugin schema and initializes the buffered send path.
func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	p.InitLogger(p.Send)

	return nil
}

// PostInit resolves expression regexps and applies defaults, metadata, and
// the batch processor.
func (p *Plugin) PostInit() error {
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
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

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) == 0 {
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.config.addr = net.JoinHostPort(p.config.Host, strconv.Itoa(p.config.Port))

	p.BatchProcessor = base.NewBatchProcessor("udp logger", base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
		PluginID:           name,
	}, p.RouteID, p.ServerAddr, p.SendBatch)

	return nil
}

// Handler logs the request after the downstream response completes, honoring
// the configured body capture expressions.
func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		request := base.CaptureMinimalAccessLogRequest(r, started)
		if len(p.LogFormat) == 0 {
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
		if len(p.LogFormat) > 0 {
			logFields = resolveUDPLogFormat(r, request, p.LogFormat)
			logFields["route_id"] = p.RouteID
			logFields["service_id"] = base.ApisixString(r, "$service_id")
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

func resolveUDPLogFormat(r *http.Request, request accessRequest, format map[string]string) map[string]any {
	return base.ResolveStringLogFormat(format, func(value string) any {
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

// Send marshals and delivers a single log entry over UDP.
func (p *Plugin) Send(log map[string]any) {
	logMessage, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in udp-logger", err)
		return
	}

	if err := p.sendBody(context.Background(), logMessage); err != nil {
		logger.Errorf("%s", err)
	}
}

// SendBatch delivers an encoded log batch over UDP.
func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	body, err := encodeBatch(entries, batchMaxSize)
	if err != nil {
		return 0, err
	}
	return 0, p.sendBody(ctx, body)
}

func encodeBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	body, err := base.EncodeLogBatch(entries, batchMaxSize, "")
	if err != nil {
		entryLabel := "entries"
		if batchMaxSize == 1 && len(entries) == 1 {
			entryLabel = "entry"
		}
		return nil, fmt.Errorf("failed to marshal udp log %s: %w", entryLabel, err)
	}
	return body, nil
}

// payloadTooLargeError reports an encoded batch that cannot fit in one UDP
// datagram; the batch processor accounts it as a failed delivery.
type payloadTooLargeError struct {
	size  int
	limit int
}

func (e *payloadTooLargeError) Error() string {
	return fmt.Sprintf("udp log payload of %d bytes exceeds the %d-byte datagram limit", e.size, e.limit)
}

func (p *Plugin) sendBody(ctx context.Context, body []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(body) > maxUDPDatagramSize {
		return &payloadTooLargeError{size: len(body), limit: maxUDPDatagramSize}
	}

	conn := p.conn
	if conn == nil {
		var err error
		conn, err = p.dial(ctx)
		if err != nil {
			return fmt.Errorf(
				"failed to connect to udp server: host[%s] port[%d]: %w",
				p.config.Host,
				p.config.Port,
				err,
			)
		}
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(time.Duration(p.config.Timeout) * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetWriteDeadline(deadline)
	stopWatcher := watchConnectionCancellation(ctx, conn)
	defer stopWatcher()
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("failed to send log message in udp-logger: %w", err)
	}
	return nil
}

func (p *Plugin) dial(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Second}
	return dialer.DialContext(ctx, "udp", p.config.addr)
}

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Go(func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	})
	return func() {
		close(done)
		wait.Wait()
	}
}
