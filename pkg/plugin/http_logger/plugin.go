package http_logger

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
)

const (
	// version  = "0.1"
	priority = 410
	name     = "http-logger"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "uri": {
		"type": "string",
		"format": "uri"
	  },
	  "auth_header": {
		"type": "string"
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
	  "concat_method": {
		"type": "string",
		"default": "json",
		"enum": ["json", "new_line"]
	  },
	  "ssl_verify": {
		"type": "boolean",
		"default": false
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
	"required": ["uri"]
}`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

type Config struct {
	URI                 string            `json:"uri"`
	AuthHeader          *string           `json:"auth_header,omitempty"`
	Timeout             int               `json:"timeout"`
	LogFormat           map[string]string `json:"log_format,omitempty"`
	SslVerify           bool              `json:"ssl_verify"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  []any             `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr []any             `json:"include_resp_body_expr,omitempty"`

	// NOTE: not needed
	ConcatMethod string `json:"concat_method"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
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
	if p.config.AuthHeader != nil {
		keyring, enabled := data_encryption.Keyring()
		resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(*p.config.AuthHeader)
		if err != nil {
			return fmt.Errorf("http-logger auth_header: %w", err)
		}
		p.config.AuthHeader = &resolved
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.ConcatMethod == "" {
		p.config.ConcatMethod = "json"
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

	// client
	configUID := shared.NewConfigUID()
	client := resty.New()

	configUID.Add(p.config.Timeout)
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	configUID.Add(p.config.SslVerify)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SslVerify})

	configUID.Add(p.config.ConcatMethod)
	if p.config.ConcatMethod == "json" {
		client.SetHeader("content-type", "application/json")
	} else {
		client.SetHeader("content-type", "text/plain")
	}
	client.SetHeader("User-Agent", "apisix-go-plugin-http-logger")

	configUID.Add(p.config.AuthHeader)
	if p.config.AuthHeader != nil {
		// we can't use  p.client.SetAuthToken here
		client.SetHeader("Authorization", *p.config.AuthHeader)
	}

	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) == 0 {
		p.LogFormat = metadata.LogFormat
	} else {
		p.LogFormat = p.config.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "http logger",
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

		logFields := make(map[string]any)
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		}
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
	body, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal log message: %s in http-logger", err)
		return
	}

	if err := p.sendBody(body); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, batchMaxSize int) (int, error) {
	body, err := p.encodeBatch(entries, batchMaxSize)
	if err != nil {
		return 0, err
	}
	return 0, p.sendBody(body)
}

func (p *Plugin) encodeBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	if p.config.ConcatMethod == "new_line" && batchMaxSize > 1 {
		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			body, err := json.Marshal(entry)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal http log entry: %w", err)
			}
			lines = append(lines, string(body))
		}
		return []byte(strings.Join(lines, "\n")), nil
	}

	if batchMaxSize == 1 && len(entries) == 1 {
		body, err := json.Marshal(entries[0])
		if err != nil {
			return nil, fmt.Errorf("failed to marshal http log entry: %w", err)
		}
		return body, nil
	}

	body, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal http log entries: %w", err)
	}
	return body, nil
}

func (p *Plugin) sendBody(body []byte) error {
	resp, err := p.client.R().SetBody(body).Post(p.config.URI)
	if err != nil {
		return fmt.Errorf("error while sending data to [%s] %s", p.config.URI, err)
	}

	if resp.StatusCode() >= 400 {
		return fmt.Errorf(
			"server returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.config.URI,
			resp.String(),
		)
	}
	return nil
}
