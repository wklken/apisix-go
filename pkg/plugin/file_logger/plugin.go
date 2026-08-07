package file_logger

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	priority          = 399
	name              = "file-logger"
	fileLoggerVersion = "apisix-go"
)

const schema = `
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "path": {
      "type": "string"
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
    "match": {
      "type": "array",
      "maxItems": 20,
      "items": {
        "anyOf": [
          {
            "type": "array"
          },
          {
            "type": "string"
          }
        ]
      }
    }
  }
}
`

const metadataSchema = `
{
  "$schema": "http://json-schema.org/draft-04/schema#",
  "type": "object",
  "properties": {
    "path": {
      "type": "string"
    },
    "log_format": {
      "type": "object"
    },
    "log_format_extra": {
      "type": "object"
    }
  }
}
`

type pluginMetadata struct {
	Path           string            `json:"path"`
	LogFormat      map[string]any    `json:"log_format"`
	LogFormatExtra map[string]string `json:"log_format_extra"`
}

type Plugin struct {
	base.BasePlugin
	config Config

	logger *zap.Logger
	writer *appendFileWriteSyncer
	lease  *fileWriterLease

	logFormat      map[string]any
	logFormatExtra map[string]string

	stopOnce sync.Once
}

type Config struct {
	Path                string            `json:"path"`
	LogFormat           map[string]any    `json:"log_format,omitempty"`
	LogFormatExtra      map[string]string `json:"log_format_extra,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any             `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any             `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	Match               []any             `json:"match,omitempty"`
}

type requestSnapshot struct {
	host       string
	remoteAddr string
	method     string
	uri        string
	headers    http.Header
	query      map[string][]string
	size       int64
	scheme     string
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema
	return nil
}

func (p *Plugin) PostInit() error {
	base.PrepareExprRegexps(p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr, p.config.Match)
	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if p.config.Path == "" {
		p.config.Path = metadata.Path
	}
	if p.config.Path == "" {
		return fmt.Errorf("file-logger path is not set in plugin config or metadata")
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
	p.config.Match = normalizeMatch(p.config.Match)

	switch {
	case p.config.LogFormat != nil:
		p.logFormat = p.config.LogFormat
	case metadata.LogFormat != nil:
		p.logFormat = metadata.LogFormat
	default:
		if p.config.LogFormatExtra != nil {
			p.logFormatExtra = p.config.LogFormatExtra
		} else {
			p.logFormatExtra = metadata.LogFormatExtra
		}
	}
	if p.logFormat != nil {
		var truncated bool
		p.logFormat, truncated = base.TruncateLogFormat(p.logFormat, 5)
		if truncated {
			logger.Warn("log_format nesting exceeds max depth 5, truncating")
		}
	}

	cfg := zap.NewProductionConfig()
	cfg.DisableCaller = true
	cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	encoder := zapcore.NewJSONEncoder(cfg.EncoderConfig)
	lease, err := sharedFileWriters.acquire(p.config.Path)
	if err != nil {
		return err
	}
	p.lease = lease
	p.writer = lease.writer
	p.logger = zap.New(zapcore.NewCore(encoder, p.writer, cfg.Level))
	return nil
}

func normalizeMatch(match []any) []any {
	if len(match) != 1 {
		return match
	}
	group, ok := match[0].([]any)
	if !ok || len(group) == 0 {
		return match
	}
	if _, ok := group[0].([]any); !ok {
		return match
	}
	return group
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.logger != nil {
			_ = p.logger.Sync()
		}
		if p.lease != nil {
			p.lease.release()
		}
	})
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		request := captureRequest(r)
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
		if !p.match(r) {
			return
		}

		includeResponseBody := recorder != nil && recorder.HasBody() &&
			base.ExprMatched(r, p.config.IncludeRespBodyExpr, metrics.Code)
		var capturedResponseBody string
		if includeResponseBody {
			capturedResponseBody = recorder.BodyDecoded(
				p.config.MaxRespBodyBytes,
				w.Header().Get("Content-Encoding"),
			)
			apisixctx.RegisterRequestVar(r, "$resp_body", capturedResponseBody)
		}
		logFields := p.buildLogFields(r, request, w.Header(), metrics, started)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if includeResponseBody {
			base.NestedLogMap(logFields, "response")["body"] = capturedResponseBody
		}

		fields := make([]zap.Field, 0, len(logFields))
		for key, value := range logFields {
			fields = append(fields, zap.Any(key, value))
		}
		p.logger.Info("", fields...)
	})
}

func captureRequest(r *http.Request) requestSnapshot {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return requestSnapshot{
		host:       base.RemoteIP(r.Host),
		remoteAddr: base.RemoteIP(r.RemoteAddr),
		method:     r.Method,
		uri:        r.URL.RequestURI(),
		headers:    r.Header.Clone(),
		query:      map[string][]string(r.URL.Query()),
		size:       max(r.ContentLength, 0),
		scheme:     scheme,
	}
}

func (p *Plugin) buildLogFields(
	r *http.Request,
	request requestSnapshot,
	responseHeaders http.Header,
	metrics httpsnoop.Metrics,
	started time.Time,
) map[string]any {
	if p.logFormat != nil {
		fields := resolveLogFormat(p.logFormat, r, request, metrics.Code)
		if routeID := base.RequestVar(r, "$route_id", metrics.Code); routeID != "" {
			fields["route_id"] = routeID
		}
		if serviceID := base.RequestVar(r, "$service_id", metrics.Code); serviceID != "" {
			fields["service_id"] = serviceID
		}
		return fields
	}

	fields := defaultLogFields(r, request, responseHeaders, metrics, started)
	for key, value := range p.logFormatExtra {
		if _, exists := fields[key]; !exists {
			fields[key] = resolveLogValue(value, r, request, metrics.Code)
		}
	}
	return fields
}

func defaultLogFields(
	r *http.Request,
	request requestSnapshot,
	responseHeaders http.Header,
	metrics httpsnoop.Metrics,
	started time.Time,
) map[string]any {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	latency := float64(time.Since(started).Microseconds()) / 1000
	fields := map[string]any{
		"request": map[string]any{
			"url":         request.scheme + "://" + request.host + request.uri,
			"uri":         request.uri,
			"method":      request.method,
			"headers":     base.CollapseHeaderValues(request.headers),
			"querystring": base.CollapseHeaderValues(http.Header(request.query)),
			"size":        request.size,
		},
		"response": map[string]any{
			"status":  metrics.Code,
			"headers": base.CollapseHeaderValues(responseHeaders),
			"size":    metrics.Written,
		},
		"server": map[string]any{
			"hostname": hostname,
			"version":  fileLoggerVersion,
		},
		"service_id":     base.RequestVar(r, "$service_id", metrics.Code),
		"route_id":       base.RequestVar(r, "$route_id", metrics.Code),
		"client_ip":      request.remoteAddr,
		"start_time":     float64(started.UnixNano()) / float64(time.Millisecond),
		"latency":        latency,
		"apisix_latency": latency,
	}
	if fields["route_id"] == "" {
		fields["route_id"] = "no-matched"
	}
	if upstream := resolvedUpstream(r); upstream != "" {
		fields["upstream"] = upstream
	}
	if consumerName := base.RequestVar(r, "$consumer_name", metrics.Code); consumerName != "" {
		fields["consumer"] = map[string]any{"username": consumerName}
	}
	return fields
}

func resolveLogFormat(
	format map[string]any,
	r *http.Request,
	request requestSnapshot,
	status int,
) map[string]any {
	return base.ResolveLogFormat(format, func(value string) any {
		return resolveLogValue(value, r, request, status)
	})
}

func resolveLogValue(value string, r *http.Request, request requestSnapshot, status int) any {
	switch value {
	case "$host":
		return request.host
	case "$remote_addr":
		return request.remoteAddr
	case "$status":
		return status
	case "$request":
		return request.method + " " + request.uri + " " + r.Proto
	case "$resp_body":
		return apisixctx.GetRequestVar(r, "$resp_body")
	case "$upstream_unresolved_host":
		return base.RequestVar(r, "$balancer_ip", status)
	default:
		return apisixlog.GetField(r, value)
	}
}

func resolvedUpstream(r *http.Request) string {
	host := base.RequestVar(r, "$balancer_ip", 0)
	if host == "" {
		return ""
	}
	if net.ParseIP(host) == nil {
		addresses, err := net.LookupIP(host)
		if err == nil {
			for _, address := range addresses {
				if ipv4 := address.To4(); ipv4 != nil {
					host = ipv4.String()
					break
				}
			}
		}
	}
	port := base.RequestVar(r, "$balancer_port", 0)
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

type appendFileWriteSyncer struct {
	path string
	mu   sync.Mutex
	file *os.File
}

func (w *appendFileWriteSyncer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to open file: %s, error info: %s", w.path, err))
			return 0, err
		}
		w.file = file
	}
	return w.file.Write(data)
}

func (w *appendFileWriteSyncer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	return w.file.Sync()
}

func (w *appendFileWriteSyncer) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *appendFileWriteSyncer) Close() error {
	return w.Reopen()
}

func (p *Plugin) match(r *http.Request) bool {
	return base.ExprMatched(r, p.config.Match, 0)
}
