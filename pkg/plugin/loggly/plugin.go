package loggly

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
}

const (
	priority = 411
	name     = "loggly"
)

const schema = `
{
  "type": "object",
  "properties": {
    "customer_token": {
      "type": "string"
    },
    "severity": {
      "type": "string",
      "default": "INFO",
      "enum": ["DEBUG", "INFO", "NOTICE", "WARNING", "ERR", "CRIT", "ALERT", "EMEGR", "debug", "info", "notice", "warning", "err", "crit", "alert", "emegr"]
    },
    "severity_map": {
      "type": "object"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
    "tags": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      },
      "default": ["apisix"]
    },
    "log_format": {
      "type": "object"
    },
    "host": {
      "type": "string",
      "default": "logs-01.loggly.com"
    },
    "port": {
      "type": "integer",
      "default": 514
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5000
    },
    "protocol": {
      "type": "string",
      "default": "syslog",
      "enum": ["syslog", "http", "https"]
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
  "required": ["customer_token"]
}
`

type pluginMetadata struct {
	Host      string            `json:"host,omitempty"`
	Port      int               `json:"port,omitempty"`
	Protocol  string            `json:"protocol,omitempty"`
	Timeout   int               `json:"timeout,omitempty"`
	LogFormat map[string]string `json:"log_format,omitempty"`
}

type Config struct {
	CustomerToken string            `json:"customer_token"`
	Severity      string            `json:"severity,omitempty"`
	SeverityMap   map[string]string `json:"severity_map,omitempty"`
	Tags          []string          `json:"tags,omitempty"`
	SSLVerify     *bool             `json:"ssl_verify,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`
	Host          string            `json:"host,omitempty"`
	Port          int               `json:"port,omitempty"`
	Timeout       int               `json:"timeout,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

var severityValues = map[string]int{
	"EMEGR":   0,
	"ALERT":   1,
	"CRIT":    2,
	"ERR":     3,
	"WARNING": 4,
	"NOTICE":  5,
	"INFO":    6,
	"DEBUG":   7,
}

func (p *Plugin) Config() any {
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
	keyring, enabled := data_encryption.Keyring()
	resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.CustomerToken)
	if err != nil {
		return fmt.Errorf("loggly customer_token: %w", err)
	}
	p.config.CustomerToken = resolved

	if p.config.Severity == "" {
		p.config.Severity = "INFO"
	}
	p.config.Severity = strings.ToUpper(p.config.Severity)
	if len(p.config.Tags) == 0 {
		p.config.Tags = []string{"apisix"}
	}
	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if p.config.Host == "" {
		p.config.Host = metadata.Host
	}
	if p.config.Host == "" {
		p.config.Host = "logs-01.loggly.com"
	}
	if p.config.Port == 0 {
		p.config.Port = metadata.Port
	}
	if p.config.Port == 0 {
		p.config.Port = 514
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = metadata.Timeout
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 5000
	}
	if p.config.Protocol == "" {
		p.config.Protocol = metadata.Protocol
	}
	if p.config.Protocol == "" {
		p.config.Protocol = "syslog"
	}
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
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

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "loggly",
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
	if !p.config.IncludeReqBody && !p.config.IncludeRespBody {
		return p.BaseLoggerPlugin.Handler(next)
	}

	fn := func(w http.ResponseWriter, r *http.Request) {
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

		next.ServeHTTP(writer, r)
		status := 0
		if recorder != nil {
			status = recorder.StatusCode()
		}

		logFields := apisixlog.GetFields(r, p.LogFormat)
		if requestBody != "" {
			base.NestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.HasBody() && base.ExprMatched(r, p.config.IncludeRespBodyExpr, status) {
			base.NestedLogMap(logFields, "response")["body"] = recorder.Body()
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch([]map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, batchMaxSize int) (int, error) {
	if p.config.Protocol == "http" || p.config.Protocol == "https" {
		return 0, p.sendHTTPBulk(entries, batchMaxSize)
	}

	for i, entry := range entries {
		message := p.buildMessage(entry)
		if err := p.sendUDPMessage(message); err != nil {
			return i + 1, err
		}
	}

	return 0, nil
}

func (p *Plugin) sendUDPMessage(message string) error {
	conn, err := net.DialTimeout(
		"udp",
		fmt.Sprintf("%s:%d", p.config.Host, p.config.Port),
		time.Duration(p.config.Timeout)*time.Millisecond,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to Loggly UDP endpoint %s:%d: %w", p.config.Host, p.config.Port, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(message)); err != nil {
		return fmt.Errorf("failed to send loggly message: %w", err)
	}
	return nil
}

func (p *Plugin) sendHTTPBulk(entries []map[string]any, batchMaxSize int) error {
	payload, err := p.encodeHTTPBulk(entries, batchMaxSize)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, p.bulkEndpoint(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to build Loggly bulk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LOGGLY-TAG", strings.Join(p.config.Tags, ","))

	client := &http.Client{
		Timeout: time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !*p.config.SSLVerify},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send loggly bulk message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to send loggly bulk message: status %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) encodeHTTPBulk(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	if batchMaxSize == 1 && len(entries) == 1 {
		payload, err := json.Marshal(entries[0])
		if err != nil {
			return nil, fmt.Errorf("failed to marshal loggly message: %w", err)
		}
		return payload, nil
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		payload, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal loggly message: %w", err)
		}
		lines = append(lines, string(payload))
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func (p *Plugin) bulkEndpoint() string {
	host := strings.TrimRight(p.config.Host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = p.config.Protocol + "://" + host
	}
	return host + "/bulk/" + p.config.CustomerToken + "/tag/bulk"
}

func (p *Plugin) buildMessage(log map[string]any) string {
	payload, err := json.Marshal(log)
	if err != nil {
		payload = []byte(`{}`)
	}

	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "-"
	}

	return strings.Join([]string{
		fmt.Sprintf("<%d>1", 8+messageSeverity(p.config.Severity, p.config.SeverityMap, log)),
		time.Now().UTC().Format(time.RFC3339Nano),
		hostname,
		"apisix",
		fmt.Sprint(os.Getpid()),
		"-",
		p.structuredData(),
		string(payload),
	}, " ")
}

func messageSeverity(defaultSeverity string, severityMap map[string]string, log map[string]any) int {
	if status, ok := log["status"]; ok {
		key := fmt.Sprint(status)
		if severity, ok := severityMap[key]; ok {
			return severityCode(severity)
		}
	}
	return severityCode(defaultSeverity)
}

func severityCode(severity string) int {
	if code, ok := severityValues[strings.ToUpper(severity)]; ok {
		return code
	}
	return severityValues["INFO"]
}

func (p *Plugin) structuredData() string {
	tags := make([]string, 0, len(p.config.Tags))
	for _, tag := range p.config.Tags {
		tags = append(tags, fmt.Sprintf(`tag="%s"`, tag))
	}
	if len(tags) == 0 {
		return fmt.Sprintf("[%s@41058]", p.config.CustomerToken)
	}
	return fmt.Sprintf("[%s@41058 %s]", p.config.CustomerToken, strings.Join(tags, " "))
}
