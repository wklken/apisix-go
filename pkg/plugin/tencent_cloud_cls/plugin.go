package tencent_cloud_cls

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/go-resty/resty/v2"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
	"google.golang.org/protobuf/encoding/protowire"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
	now    func() time.Time
}

const (
	priority = 397
	name     = "tencent-cloud-cls"

	defaultScheme      = "https"
	clsAPIPath         = "/structuredlog"
	authExpireSeconds  = 60
	defaultHTTPTimeout = 10 * time.Second

	maxSingleValueSize   = 1 * 1024 * 1024
	maxLogGroupValueSize = 5 * 1024 * 1024
)

const schema = `
{
  "type": "object",
  "properties": {
    "cls_host": {
      "type": "string"
    },
    "cls_topic": {
      "type": "string"
    },
    "scheme": {
      "type": "string",
      "enum": ["http", "https"],
      "default": "https"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "secret_id": {
      "type": "string"
    },
    "secret_key": {
      "type": "string"
    },
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1,
      "default": 1
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
    "global_tag": {
      "type": "object"
    },
    "log_format": {
      "type": "object"
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
  "required": ["cls_host", "cls_topic", "secret_id", "secret_key"]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Config struct {
	CLSHost             string            `json:"cls_host"`
	CLSTopic            string            `json:"cls_topic"`
	Scheme              string            `json:"scheme,omitempty"`
	SSLVerify           *bool             `json:"ssl_verify,omitempty"`
	SecretID            string            `json:"secret_id"`
	SecretKey           string            `json:"secret_key"`
	SampleRatio         float64           `json:"sample_ratio,omitempty"`
	IncludeReqBody      bool              `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any           `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool              `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any           `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int               `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int               `json:"max_resp_body_bytes,omitempty"`
	GlobalTag           map[string]string `json:"global_tag,omitempty"`
	LogFormat           map[string]string `json:"log_format,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
	Timeout           int `json:"-"`
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
	resolved, err := data_encryption.NewResolver(enabled, keyring).Resolve(p.config.SecretKey)
	if err != nil {
		return fmt.Errorf("tencent-cloud-cls secret_key: %w", err)
	}
	p.config.SecretKey = resolved

	p.applyDefaults()

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.Scheme)
	configUID.Add(p.config.CLSHost)
	configUID.Add(p.config.CLSTopic)
	configUID.Add(p.config.SecretID)
	configUID.Add(p.sslVerify())
	configUID.Add(p.config.Timeout)

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.sslVerify()})
	p.client = shared.LoadOrStoreClient(name, configUID, client).(*resty.Client)

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "tencent-cloud-cls",
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
	if p.config.IncludeReqBody || p.config.IncludeRespBody {
		return p.bodyAwareHandler(next)
	}

	if p.config.SampleRatio >= 1 {
		return p.BaseLoggerPlugin.Handler(next)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if rand.Float64() >= p.config.SampleRatio {
			return
		}
		_ = p.Fire(apisixlog.GetFields(r, p.LogFormat))
	})
}

func (p *Plugin) bodyAwareHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		sampled := p.config.SampleRatio >= 1 || rand.Float64() < p.config.SampleRatio

		var requestBody string
		if sampled && p.config.IncludeReqBody && base.ExprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := base.ReadAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *base.ResponseRecorder
		if sampled && p.config.IncludeRespBody {
			recorder = base.NewResponseRecorder(w, p.config.MaxRespBodyBytes)
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		if !sampled {
			return
		}
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
	_ = batchMaxSize

	payload := p.buildBatchPayload(entries)
	if len(payload) == 0 {
		return 0, nil
	}

	resp, err := p.client.R().
		SetHeader("Host", p.config.CLSHost).
		SetHeader("Content-Type", "application/x-protobuf").
		SetHeader("Authorization", p.authorization()).
		SetBody(payload).
		Post(p.endpointURL())
	if err != nil {
		return 0, fmt.Errorf("failed to send log to Tencent Cloud CLS endpoint %s: %w", p.endpointURL(), err)
	}
	if resp.StatusCode() >= 300 {
		return 0, fmt.Errorf(
			"tencent Cloud CLS endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			p.endpointURL(),
			resp.String(),
		)
	}
	return 0, nil
}

func (p *Plugin) applyDefaults() {
	if p.config.Scheme == "" {
		p.config.Scheme = defaultScheme
	}
	if p.config.SSLVerify == nil {
		verify := true
		p.config.SSLVerify = &verify
	}
	if p.config.SampleRatio == 0 {
		p.config.SampleRatio = 1
	}
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = int(defaultHTTPTimeout / time.Millisecond)
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
	if p.now == nil {
		p.now = time.Now
	}
}

func (p *Plugin) sslVerify() bool {
	return p.config.SSLVerify == nil || *p.config.SSLVerify
}

func (p *Plugin) endpointURL() string {
	values := url.Values{}
	values.Set("topic_id", p.config.CLSTopic)
	return fmt.Sprintf("%s://%s%s?%s", p.config.Scheme, p.config.CLSHost, clsAPIPath, values.Encode())
}

func (p *Plugin) authorization() string {
	signTime := fmt.Sprintf("%d;%d", p.now().Unix(), p.now().Unix()+authExpireSeconds)
	httpRequestInfo := fmt.Sprintf("%s\n%s\n%s\n%s\n", "post", clsAPIPath, "", "")
	stringToSign := fmt.Sprintf("%s\n%s\n%s\n", "sha1", signTime, sha1Hex([]byte(httpRequestInfo)))
	signKey := hmacSHA1Hex([]byte(p.config.SecretKey), []byte(signTime))
	signature := hmacSHA1Hex([]byte(signKey), []byte(stringToSign))

	return "q-sign-algorithm=sha1" +
		"&q-ak=" + p.config.SecretID +
		"&q-sign-time=" + signTime +
		"&q-key-time=" + signTime +
		"&q-header-list=" +
		"&q-url-param-list=" +
		"&q-signature=" + signature
}

func (p *Plugin) buildBatchPayload(logs []map[string]any) []byte {
	group := []byte(nil)
	totalSize := 0
	for _, logEntry := range logs {
		contents, size := normalizeLog(logEntry, p.config.GlobalTag)
		if size > maxLogGroupValueSize {
			logger.Errorf("Tencent Cloud CLS log size is over 5MB, dropped")
			continue
		}
		totalSize += size
		if totalSize > maxLogGroupValueSize {
			logger.Errorf("Tencent Cloud CLS batch size is over 5MB, dropped")
			break
		}
		group = appendBytesField(group, 1, appendLog(nil, p.now().UnixMilli(), contents))
	}
	if len(group) == 0 {
		return nil
	}
	if hostname, err := os.Hostname(); err == nil && hostname != "" {
		group = appendStringField(group, 4, hostname)
	}
	return appendBytesField(nil, 1, group)
}

type clsContent struct {
	key   string
	value string
}

func normalizeLog(log map[string]any, globalTag map[string]string) ([]clsContent, int) {
	contents := make([]clsContent, 0, len(log)+len(globalTag))
	size := 4
	for key, value := range log {
		normalized := normalizeValue(value)
		if len(normalized) > maxSingleValueSize {
			normalized = normalized[:maxSingleValueSize]
		}
		contents = append(contents, clsContent{key: key, value: normalized})
		size += len(key) + len(normalized)
	}
	for key, value := range globalTag {
		contents = append(contents, clsContent{key: key, value: value})
		size += len(key) + len(value)
	}
	return contents, size
}

func normalizeValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case fmt.Stringer:
		return v.String()
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case bool:
		return strconv.FormatBool(v)
	default:
		if payload, err := json.Marshal(v); err == nil {
			return string(payload)
		}
		return fmt.Sprint(v)
	}
}

func appendLog(buf []byte, timestamp int64, contents []clsContent) []byte {
	buf = protowire.AppendTag(buf, 1, protowire.VarintType)
	buf = protowire.AppendVarint(buf, uint64(timestamp))
	for _, content := range contents {
		raw := appendStringField(nil, 1, content.key)
		raw = appendStringField(raw, 2, content.value)
		buf = appendBytesField(buf, 2, raw)
	}
	return buf
}

func appendStringField(buf []byte, number protowire.Number, value string) []byte {
	return appendBytesField(buf, number, []byte(value))
}

func appendBytesField(buf []byte, number protowire.Number, value []byte) []byte {
	buf = protowire.AppendTag(buf, number, protowire.BytesType)
	buf = protowire.AppendBytes(buf, value)
	return buf
}

func sha1Hex(value []byte) string {
	sum := sha1.Sum(value)
	return hex.EncodeToString(sum[:])
}

func hmacSHA1Hex(key []byte, value []byte) string {
	mac := hmac.New(sha1.New, key)
	mac.Write(value)
	return hex.EncodeToString(mac.Sum(nil))
}
