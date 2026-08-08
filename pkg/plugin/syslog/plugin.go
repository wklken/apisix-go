package syslog

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

const (
	priority       = 401
	name           = "syslog"
	version        = "apisix-go"
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

	var parsed struct {
		config
		RetryDelay     json.RawMessage `json:"retry_delay"`
		LogFormat      json.RawMessage `json:"log_format"`
		LogFormatExtra json.RawMessage `json:"log_format_extra"`
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
	if len(parsed.LogFormat) > 0 {
		c.logFormatSet = true
		if string(parsed.LogFormat) != "null" {
			if err := json.Unmarshal(parsed.LogFormat, &c.LogFormat); err != nil {
				return err
			}
		}
	}
	if len(parsed.LogFormatExtra) > 0 {
		c.logFormatExtraSet = true
		if string(parsed.LogFormatExtra) != "null" {
			if err := json.Unmarshal(parsed.LogFormatExtra, &c.LogFormatExtra); err != nil {
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
	p.logFormat, truncated = base.TruncateLogFormat(p.logFormat, 5)
	var extraTruncated bool
	p.logFormatExtra, extraTruncated = base.TruncateLogFormat(p.logFormatExtra, 5)
	if truncated || extraTruncated {
		logger.Warn("log_format nesting exceeds max depth 5, truncating")
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	if p.config.SockType == "" {
		p.config.SockType = "tcp"
	}

	p.config.addr = net.JoinHostPort(p.config.Host, strconv.Itoa(p.config.Port))
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
		request := base.CaptureMinimalAccessLogRequest(r, started)
		if !p.customLogFormat {
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
		if p.customLogFormat {
			logFields = resolveSyslogLogFormat(r, request, p.logFormat)
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
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
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

type accessRequest = base.AccessLogRequest

func resolveSyslogLogFormat(
	r *http.Request,
	request accessRequest,
	format map[string]any,
) map[string]any {
	return base.ResolveLogFormat(format, func(value string) any {
		switch value {
		case "$host":
			return request.Host
		case "$remote_addr":
			return request.ClientIP
		case "$time_iso8601":
			return request.Started.Format(time.RFC3339)
		case "$upstream_addr":
			return base.UpstreamAddress(r)
		default:
			return apisixlog.GetField(r, value)
		}
	})
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
