package clickhouse_logger

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client

	clientRelease  func()
	userSecret     *store.ResolvedSecret
	passwordSecret *store.ResolvedSecret
}

const (
	priority = 398
	name     = "clickhouse-logger"
)

var randomEndpointIndex = rand.Intn

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addr": {
      "type": "string",
      "format": "uri"
    },
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "user": {
      "type": "string",
      "default": ""
    },
    "password": {
      "type": "string",
      "default": ""
    },
    "database": {
      "type": "string",
      "default": ""
    },
    "logtable": {
      "type": "string",
      "default": ""
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "name": {
      "type": "string",
      "default": "clickhouse logger"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
  "oneOf": [
    {"required": ["endpoint_addr", "user", "password", "database", "logtable"]},
    {"required": ["endpoint_addrs", "user", "password", "database", "logtable"]}
  ]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Config struct {
	EndpointAddr  string            `json:"endpoint_addr,omitempty"`
	EndpointAddrs []string          `json:"endpoint_addrs,omitempty"`
	User          string            `json:"user"`
	Password      string            `json:"password"`
	Database      string            `json:"database"`
	LogTable      string            `json:"logtable"`
	Timeout       int               `json:"timeout,omitempty"`
	Name          string            `json:"name,omitempty"`
	SSLVerify     *bool             `json:"ssl_verify,omitempty"`
	LogFormat     map[string]string `json:"log_format,omitempty"`

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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	p.InitLogger(p.Send)

	return nil
}

func (p *Plugin) PostInit() error {
	if !p.DataEncryption().Configured() {
		return errors.New("data-encryption resolver is required")
	}
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if p.passwordSecret == nil {
		resolved, err := p.DataEncryption().ResolveForContext(
			p.config.Password,
			"clickhouse-logger.password",
		)
		if err != nil {
			return fmt.Errorf("clickhouse-logger password: %w", err)
		}
		p.config.Password = resolved
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
	logFormat, err := base.RequireStringLogFormat(name, p.config.LogFormat, metadata.LogFormat)
	if err != nil {
		return err
	}
	p.LogFormat = logFormat

	configUID := shared.NewConfigUID()
	configUID.Add(p.endpointUID())
	configUID.Add(p.config.User)
	configUID.Add(p.config.Password)
	configUID.Add(p.config.Database)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.sslVerify())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Second)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) { return client, nil },
		shared.CloseRestyClient,
	)
	if err != nil {
		return err
	}
	p.client = value.(*resty.Client)
	p.clientRelease = release

	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	p.BatchProcessor = base.NewBatchProcessor("clickhouse logger", base.BatchDefaults{
		PluginID:           name,
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
	}, p.RouteID, p.ServerAddr, p.SendBatch)

	return nil
}

func (p *Plugin) MaterializeSecrets() error {
	userSecret, err := materializeSecretReference(p.config.User)
	if err != nil {
		return err
	}
	passwordSecret, err := materializeSecretReference(p.config.Password)
	if err != nil {
		userSecret.Destroy()
		return err
	}
	p.userSecret = userSecret
	p.passwordSecret = passwordSecret
	if userSecret != nil {
		p.config.User = userSecret.Descriptor()
	}
	if passwordSecret != nil {
		p.config.Password = passwordSecret.Descriptor()
	}
	return nil
}

func materializeSecretReference(value string) (*store.ResolvedSecret, error) {
	upper := strings.ToUpper(value)
	if !strings.HasPrefix(upper, "$ENV://") && !strings.HasPrefix(value, "$secret://") {
		return nil, nil
	}
	secret, err := store.MaterializeSecret(value)
	if err != nil {
		return nil, err
	}
	return secret, nil
}

func (p *Plugin) resolvedUser() string {
	if p.userSecret != nil {
		return string(p.userSecret.Bytes())
	}
	return p.config.User
}

func (p *Plugin) resolvedPassword() string {
	if p.passwordSecret != nil {
		return string(p.passwordSecret.Bytes())
	}
	return p.config.Password
}

func (p *Plugin) Stop() {
	p.StopWithCleanup(func() {
		if p.clientRelease != nil {
			p.clientRelease()
			p.clientRelease = nil
		}
		p.userSecret.Destroy()
		p.userSecret = nil
		p.passwordSecret.Destroy()
		p.passwordSecret = nil
	})
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if !p.config.IncludeReqBody && !p.config.IncludeRespBody {
		return p.BaseLoggerPlugin.Handler(next)
	}

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
			base.NestedLogMap(logFields, "response")["body"] = recorder.BodyTruncated(p.config.MaxRespBodyBytes)
		}

		_ = p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	endpoint := p.endpointURL()
	if endpoint == "" {
		return 0, fmt.Errorf("clickhouse-logger endpoint is empty")
	}

	body, err := p.buildInsertBody(entries, batchMaxSize)
	if err != nil {
		return 0, err
	}

	resp, err := p.client.R().
		SetContext(ctx).
		SetHeaders(map[string]string{
			"Content-Type":          "application/json",
			"X-ClickHouse-User":     p.resolvedUser(),
			"X-ClickHouse-Key":      p.resolvedPassword(),
			"X-ClickHouse-Database": p.config.Database,
		}).
		SetBody(body).
		Post(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to send log to ClickHouse endpoint %s: %w", endpoint, err)
	}

	if resp.StatusCode() >= 400 {
		return 0, fmt.Errorf(
			"ClickHouse endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}

	return 0, nil
}

func (p *Plugin) buildInsertBody(entries []map[string]any, batchMaxSize int) (string, error) {
	if batchMaxSize == 1 && len(entries) == 1 {
		payload, err := json.Marshal(entries[0])
		if err != nil {
			return "", fmt.Errorf("failed to marshal clickhouse log entry: %w", err)
		}
		return "INSERT INTO " + p.config.LogTable + " FORMAT JSONEachRow " + string(payload), nil
	}

	rows := make([]string, 0, len(entries))
	for _, entry := range entries {
		payload, err := json.Marshal(entry)
		if err != nil {
			return "", fmt.Errorf("failed to marshal clickhouse log entry: %w", err)
		}
		rows = append(rows, string(payload))
	}
	return "INSERT INTO " + p.config.LogTable + " FORMAT JSONEachRow " + strings.Join(rows, " "), nil
}

func (p *Plugin) endpointURL() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	if len(p.config.EndpointAddrs) == 0 {
		return ""
	}
	return p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
}

func (p *Plugin) endpointUID() string {
	if p.config.EndpointAddr != "" {
		return p.config.EndpointAddr
	}
	return strings.Join(p.config.EndpointAddrs, "\x00")
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}
