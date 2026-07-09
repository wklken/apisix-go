package tencent_cloud_cls

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/shared"
	"github.com/wklken/apisix-go/pkg/store"
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

func (p *Plugin) Config() interface{} {
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

	metadata := loadMetadata()
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
		p.Fire(apisixlog.GetFields(r, p.LogFormat))
	})
}

func (p *Plugin) bodyAwareHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		sampled := p.config.SampleRatio >= 1 || rand.Float64() < p.config.SampleRatio

		var requestBody string
		if sampled && p.config.IncludeReqBody && exprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *clsLogResponseRecorder
		if sampled && p.config.IncludeRespBody {
			recorder = &clsLogResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		if !sampled {
			return
		}
		status := 0
		if recorder != nil {
			status = recorder.status
		}

		logFields := apisixlog.GetFields(r, p.LogFormat)
		if requestBody != "" {
			nestedLogMap(logFields, "request")["body"] = requestBody
		}
		if recorder != nil && recorder.body.Len() > 0 && exprMatched(r, p.config.IncludeRespBodyExpr, status) {
			nestedLogMap(logFields, "response")["body"] = recorder.body.String()
		}
		p.Fire(logFields)
	}
	return http.HandlerFunc(fn)
}

type clsLogResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *clsLogResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *clsLogResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *clsLogResponseRecorder) capture(body []byte) {
	if w.limit <= 0 || w.body.Len() >= w.limit {
		return
	}
	remaining := w.limit - w.body.Len()
	if len(body) > remaining {
		body = body[:remaining]
	}
	_, _ = w.body.Write(body)
}

func readAndRestoreRequestBody(r *http.Request, limit int) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if limit > 0 && len(body) > limit {
		body = body[:limit]
	}
	return string(body), nil
}

func nestedLogMap(fields map[string]any, key string) map[string]any {
	if value, ok := fields[key].(map[string]any); ok {
		return value
	}
	value := map[string]any{}
	fields[key] = value
	return value
}

func exprMatched(r *http.Request, exprs [][]any, status int) bool {
	if len(exprs) == 0 {
		return true
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range exprs {
		if len(condition) == 1 {
			if op, ok := condition[0].(string); ok {
				switch strings.ToUpper(op) {
				case "AND", "OR":
					pendingOp = strings.ToUpper(op)
				default:
					return false
				}
				continue
			}
		}

		matched := matchCondition(r, condition, status)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}

		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func matchCondition(r *http.Request, condition []any, status int) bool {
	if len(condition) != 3 {
		return false
	}

	left := fmt.Sprint(condition[0])
	op := fmt.Sprint(condition[1])
	right := fmt.Sprint(condition[2])
	actual := requestVar(r, left, status)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

func requestVar(r *http.Request, name string, status int) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code":
		if status > 0 {
			return strconv.Itoa(status)
		}
		return fmt.Sprint(apisixctx.GetRequestVar(r, "$status"))
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
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
			"Tencent Cloud CLS endpoint returned status code [%d] uri [%s], body [%s]",
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

func (p *Plugin) buildPayload(log map[string]any) []byte {
	return p.buildBatchPayload([]map[string]any{log})
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

func loadMetadata() (metadata pluginMetadata) {
	defer func() {
		if recover() != nil {
			metadata = pluginMetadata{}
		}
	}()

	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return pluginMetadata{}
	}
	return metadata
}
