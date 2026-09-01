package file_logger

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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

	lease     *fileWriterLease
	processor *fileLoggerProcessor

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
	var metadata pluginMetadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("file-logger metadata decode failed: %w", err)
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr, p.config.Match,
	); err != nil {
		return err
	}
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

	lease, err := sharedFileWriters.acquire(p.config.Path)
	if err != nil {
		return err
	}
	processor, err := newFileLoggerProcessor(p.TaskOwner(), lease.writer)
	if err != nil {
		lease.release()
		return err
	}
	processor.snapshotFields = p.buildSnapshotFields
	p.lease = lease
	p.processor = processor
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
		cleanup := func() {
			if p.lease != nil {
				p.lease.release()
			}
		}
		if p.processor != nil {
			p.processor.stopWithCleanup(cleanup)
		} else {
			cleanup()
		}
	})
}

func (p *Plugin) QuiesceGenerationTasks() {
	p.Stop()
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *Plugin) LogCapturePolicy() base.LogCapturePolicy {
	policy := base.LogCapturePolicyForFormats(
		p.config.MaxReqBodyBytes,
		p.config.MaxRespBodyBytes,
		p.logFormat,
		p.logFormatExtra,
	)
	if p.config.IncludeReqBody {
		policy.RequestBodyBytes = p.config.MaxReqBodyBytes
	}
	if p.config.IncludeRespBody {
		policy.ResponseBodyBytes = p.config.MaxRespBodyBytes
	}
	return policy
}

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if !base.SnapshotExpressionMatches(snapshot, p.config.Match) {
		return nil
	}
	if p.processor == nil {
		return base.ErrLogQueueUnavailable
	}
	return p.processor.pushSnapshot(snapshot)
}

func (p *Plugin) buildSnapshotFields(snapshot base.LogSnapshot) map[string]any {
	var fields map[string]any
	requestBodyVisible := p.config.IncludeReqBody &&
		base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr)
	responseBodyVisible := p.config.IncludeRespBody &&
		base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr)
	if p.logFormat != nil {
		fields = base.ResolveLogFormat(p.logFormat, func(value string) any {
			return fileSnapshotValue(
				snapshot,
				value,
				requestBodyVisible,
				responseBodyVisible,
				p.config.MaxRespBodyBytes,
			)
		})
		if routeID := fmt.Sprint(base.SnapshotValue(snapshot, "$route_id")); routeID != "" {
			fields["route_id"] = routeID
		}
		if serviceID := fmt.Sprint(base.SnapshotValue(snapshot, "$service_id")); serviceID != "" {
			fields["service_id"] = serviceID
		}
	} else {
		fields = snapshotDefaultLogFields(snapshot)
		for key, value := range p.logFormatExtra {
			if _, exists := fields[key]; !exists {
				fields[key] = fileSnapshotValue(
					snapshot,
					value,
					requestBodyVisible,
					responseBodyVisible,
					p.config.MaxRespBodyBytes,
				)
			}
		}
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
	return fields
}

func fileSnapshotValue(
	snapshot base.LogSnapshot,
	value string,
	requestBodyVisible bool,
	responseBodyVisible bool,
	responseLimit int,
) any {
	switch value {
	case "$request":
		return snapshot.Request.Method + " " + snapshot.Request.URI + " " + snapshot.Request.Proto
	case "$request_body":
		if requestBodyVisible {
			return base.SnapshotRequestBody(snapshot, len(snapshot.Request.Body))
		}
		return ""
	case "$resp_body", "$response_body":
		if responseBodyVisible {
			return base.SnapshotResponseBody(snapshot, responseLimit)
		}
		return ""
	case "$upstream_unresolved_host":
		return base.SnapshotValue(snapshot, "$balancer_ip")
	default:
		return base.SnapshotValue(snapshot, value)
	}
}

func fileRequestHeaders(headers http.Header, host string) map[string]any {
	collapsed := base.CollapseAccessLogHeaderValues(headers)
	collapsed["host"] = host
	return collapsed
}

func snapshotDefaultLogFields(snapshot base.LogSnapshot) map[string]any {
	hostname := base.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	latency := float64(0)
	if !snapshot.Started.IsZero() && !snapshot.Finished.IsZero() {
		latency = float64(snapshot.Finished.Sub(snapshot.Started).Microseconds()) / 1000
	}
	fields := map[string]any{
		"request": map[string]any{
			"url": snapshot.Request.Scheme + "://" + base.RemoteIP(snapshot.Request.Host) + snapshot.Request.URI,
			"uri": snapshot.Request.URI, "method": snapshot.Request.Method,
			"headers":     fileRequestHeaders(snapshot.Request.Header, snapshot.Request.Host),
			"querystring": base.CollapseHeaderValues(http.Header(snapshot.Request.Query)),
			"size":        max(snapshot.Request.ContentLength, 0),
		},
		"response": map[string]any{
			"status": snapshot.Outcome.Status, "headers": base.CollapseAccessLogHeaderValues(snapshot.Response.Header),
			"size": snapshot.Outcome.Bytes,
		},
		"server":     map[string]any{"hostname": hostname, "version": fileLoggerVersion},
		"service_id": base.SnapshotValue(snapshot, "$service_id"),
		"route_id":   base.SnapshotValue(snapshot, "$route_id"),
		"client_ip":  base.RemoteIP(snapshot.Request.RemoteAddr),
		"start_time": float64(snapshot.Started.UnixNano()) / float64(time.Millisecond),
		"latency":    latency, "apisix_latency": latency,
	}
	if fields["route_id"] == "" {
		fields["route_id"] = "no-matched"
	}
	if snapshot.Request.Consumer.Username != "" {
		fields["consumer"] = map[string]any{"username": snapshot.Request.Consumer.Username}
	}
	if upstream := resolvedSnapshotUpstream(snapshot); upstream != "" {
		fields["upstream"] = upstream
	}
	return fields
}

func resolvedSnapshotUpstream(snapshot base.LogSnapshot) string {
	host := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_ip"))
	if host == "" {
		return ""
	}
	if net.ParseIP(host) == nil {
		lookupCtx, cancel := context.WithTimeout(context.Background(), fileLoggerBatchMaxDelay)
		addresses, err := fileLoggerLookupIP(lookupCtx, host)
		cancel()
		if err == nil {
			for _, address := range addresses {
				if ipv4 := address.IP.To4(); ipv4 != nil {
					host = ipv4.String()
					break
				}
			}
		}
	}
	port := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_port"))
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

var fileLoggerLookupIP = net.DefaultResolver.LookupIPAddr

type appendFileWriteSyncer struct {
	path string
	mu   sync.Mutex
	file *os.File
}

func (w *appendFileWriteSyncer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		file, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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
