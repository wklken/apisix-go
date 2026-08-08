package sls_logger

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	addr string
}

const (
	priority = 406
	name     = "sls-logger"
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
    "project": {
      "type": "string"
    },
    "logstore": {
      "type": "string"
    },
    "access_key_id": {
      "type": "string"
    },
    "access_key_secret": {
      "type": "string"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
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
    }
  },
  "required": ["host", "port", "project", "logstore", "access_key_id", "access_key_secret"]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "log_format": {
      "type": "object"
    }
  }
}
`

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type Config struct {
	Host            string            `json:"host"`
	Port            int               `json:"port"`
	Project         string            `json:"project"`
	Logstore        string            `json:"logstore"`
	AccessKeyID     string            `json:"access_key_id"`
	AccessKeySecret string            `json:"access_key_secret"`
	SSLVerify       *bool             `json:"ssl_verify,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	LogFormat       map[string]string `json:"log_format,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize    int `json:"batch_max_size,omitempty"`
	MaxRetryCount   int `json:"max_retry_count,omitempty"`
	RetryDelay      int `json:"retry_delay,omitempty"`
	BufferDuration  int `json:"buffer_duration,omitempty"`
	InactiveTimeout int `json:"inactive_timeout,omitempty"`
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
	keyring, enabled := data_encryption.Keyring()
	resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.AccessKeySecret)
	if err != nil {
		return fmt.Errorf("sls-logger access_key_secret: %w", err)
	}
	p.config.AccessKeySecret = resolved

	if p.config.SSLVerify == nil {
		verify := true
		p.config.SSLVerify = &verify
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 5000
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
	p.addr = net.JoinHostPort(p.config.Host, strconv.Itoa(p.config.Port))

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = base.LoadPluginMetadata[pluginMetadata](name).LogFormat
	}

	p.BatchProcessor = base.NewBatchProcessor("sls logger", base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
	}, p.RouteID, p.ServerAddr, p.SendBatch)

	return nil
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

		metrics := httpsnoop.CaptureMetrics(next, writer, r)
		status := metrics.Code

		var logFields map[string]any
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		} else {
			logFields = defaultAccessLogFields(r, status, w.Header())
		}
		logFields["route_id"] = p.RouteID
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func defaultAccessLogFields(r *http.Request, status int, responseHeaders http.Header) map[string]any {
	requestHeaders := headerFields(r.Header)
	requestHeaders["host"] = r.Host
	return map[string]any{
		"client_ip": base.RemoteIP(r.RemoteAddr),
		"request": map[string]any{
			"method":      r.Method,
			"uri":         r.URL.RequestURI(),
			"headers":     requestHeaders,
			"querystring": queryFields(r),
		},
		"response": map[string]any{
			"status":  status,
			"headers": headerFields(responseHeaders),
		},
	}
}

func headerFields(header http.Header) map[string]any {
	fields := make(map[string]any, len(header))
	for name, values := range header {
		key := strings.ToLower(name)
		if len(values) == 1 {
			fields[key] = values[0]
		} else {
			fields[key] = append([]string(nil), values...)
		}
	}
	return fields
}

func queryFields(r *http.Request) map[string]any {
	query := r.URL.Query()
	fields := make(map[string]any, len(query))
	for name, values := range query {
		if len(values) == 1 {
			fields[name] = values[0]
		} else {
			fields[name] = append([]string(nil), values...)
		}
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

	messages := make([]string, 0, len(entries))
	for _, entry := range entries {
		messages = append(messages, p.buildMessage(entry))
	}
	return 0, p.sendMessage(strings.Join(messages, ""))
}

func (p *Plugin) sendMessage(message string) error {
	dialer := &net.Dialer{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: p.config.Host,
	}
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // explicit APISIX-compatible operator opt-out
	}
	conn, err := tls.DialWithDialer(dialer, "tcp", p.addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("failed to connect to SLS TLS endpoint %s: %w", p.addr, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("failed to send SLS log message: %w", err)
	}
	return nil
}

func (p *Plugin) buildMessage(log map[string]any) string {
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}

	hostname := base.Hostname()
	if hostname == "" {
		hostname = "-"
	}

	return strings.Join([]string{
		"<46>1",
		time.Now().UTC().Format(time.RFC3339Nano),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(),
		string(payload),
	}, " ") + "\n"
}

func (p *Plugin) structuredData() string {
	return fmt.Sprintf(
		`[logservice project="%s" logstore="%s" access-key-id="%s" access-key-secret="%s"]`,
		escapeStructuredDataValue(p.config.Project),
		escapeStructuredDataValue(p.config.Logstore),
		escapeStructuredDataValue(p.config.AccessKeyID),
		escapeStructuredDataValue(p.config.AccessKeySecret),
	)
}

func escapeStructuredDataValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `]`, `\]`)
	return value
}
