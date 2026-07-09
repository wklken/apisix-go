package loki_logger

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
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
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config

	client *resty.Client
}

const (
	priority = 414
	name     = "loki-logger"
)

var randomEndpointIndex = rand.Intn

const schema = `
{
  "type": "object",
  "properties": {
    "endpoint_addrs": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string",
        "format": "uri"
      }
    },
    "endpoint_uri": {
      "type": "string",
      "minLength": 1,
      "default": "/loki/api/v1/push"
    },
    "tenant_id": {
      "type": "string",
      "default": "fake"
    },
    "headers": {
      "type": "object"
    },
    "log_labels": {
      "type": "object",
      "default": {
        "job": "apisix"
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
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
  "required": ["endpoint_addrs"]
}
`

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type Config struct {
	EndpointAddrs    []string          `json:"endpoint_addrs"`
	EndpointURI      string            `json:"endpoint_uri,omitempty"`
	TenantID         string            `json:"tenant_id,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	LogLabels        map[string]string `json:"log_labels,omitempty"`
	SSLVerify        bool              `json:"ssl_verify"`
	Timeout          int               `json:"timeout,omitempty"`
	Keepalive        *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int               `json:"keepalive_pool,omitempty"`
	LogFormat        map[string]string `json:"log_format,omitempty"`

	IncludeReqBody     bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr [][]any `json:"include_req_body_expr,omitempty"`

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

type lokiPayload struct {
	Streams []lokiStream `json:"streams"`
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
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
	if p.config.EndpointURI == "" {
		p.config.EndpointURI = "/loki/api/v1/push"
	}
	if p.config.TenantID == "" {
		p.config.TenantID = "fake"
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}
	if len(p.config.LogLabels) == 0 {
		p.config.LogLabels = map[string]string{"job": "apisix"}
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

	configUID := shared.NewConfigUID()
	configUID.Add(p.config.EndpointAddrs)
	configUID.Add(p.config.EndpointURI)
	configUID.Add(p.config.TenantID)
	configUID.Add(p.config.Headers)
	configUID.Add(p.config.Timeout)
	configUID.Add(p.config.SSLVerify)
	configUID.Add(p.keepalive())

	client := resty.New()
	client.SetTimeout(time.Duration(p.config.Timeout) * time.Millisecond)
	client.SetTLSClientConfig(&tls.Config{InsecureSkipVerify: !p.config.SSLVerify})
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
		Name:              "loki logger",
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
		if p.config.IncludeReqBody && exprMatched(r, p.config.IncludeReqBodyExpr, 0) {
			body, err := readAndRestoreRequestBody(r, p.config.MaxReqBodyBytes)
			if err == nil && body != "" {
				requestBody = body
			}
		}

		writer := w
		var recorder *lokiResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &lokiResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)
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

type lokiResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *lokiResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *lokiResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *lokiResponseRecorder) capture(body []byte) {
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

	if len(p.config.EndpointAddrs) == 0 {
		return 0, fmt.Errorf("loki-logger endpoint_addrs is empty")
	}

	endpoint := p.endpointURL()
	resp, err := p.client.R().
		SetHeaders(p.headers()).
		SetBody(p.buildBatchPayload(entries)).
		Post(endpoint)
	if err != nil {
		return 0, fmt.Errorf("failed to send log to Loki endpoint %s: %w", endpoint, err)
	}

	if resp.StatusCode() >= 300 {
		return 0, fmt.Errorf(
			"Loki endpoint returned status code [%d] uri [%s], body [%s]",
			resp.StatusCode(),
			endpoint,
			resp.String(),
		)
	}

	return 0, nil
}

func (p *Plugin) buildPayload(log map[string]any) lokiPayload {
	return p.buildBatchPayload([]map[string]any{log})
}

func (p *Plugin) buildBatchPayload(entries []map[string]any) lokiPayload {
	values := make([][2]string, 0, len(entries))
	for _, logEntry := range entries {
		logTime := fmt.Sprintf("%d", time.Now().UnixNano())
		if value, ok := logEntry["loki_log_time"]; ok {
			logTime = fmt.Sprint(value)
		}

		entry := logEntry
		if _, ok := logEntry["loki_log_time"]; ok {
			entry = make(map[string]any, len(logEntry)-1)
			for key, value := range logEntry {
				if key != "loki_log_time" {
					entry[key] = value
				}
			}
		}

		body, err := json.Marshal(entry)
		if err != nil {
			body = []byte(`{}`)
		}
		values = append(values, [2]string{logTime, string(body)})
	}

	labels := map[string]string{}
	if len(entries) > 0 {
		labels = p.resolveLabels(entries[0])
	}
	return lokiPayload{
		Streams: []lokiStream{
			{
				Stream: labels,
				Values: values,
			},
		},
	}
}

func (p *Plugin) resolveLabels(log map[string]any) map[string]string {
	labels := make(map[string]string, len(p.config.LogLabels))
	for key, value := range p.config.LogLabels {
		if strings.HasPrefix(value, "$") {
			if resolved, ok := log[strings.TrimPrefix(value, "$")]; ok {
				labels[key] = fmt.Sprint(resolved)
				continue
			}
		}
		labels[key] = value
	}
	return labels
}

func (p *Plugin) headers() map[string]string {
	headers := make(map[string]string, len(p.config.Headers)+2)
	for key, value := range p.config.Headers {
		if strings.EqualFold(key, "X-Scope-OrgID") || strings.EqualFold(key, "Content-Type") {
			continue
		}
		headers[key] = value
	}
	headers["X-Scope-OrgID"] = p.config.TenantID
	headers["Content-Type"] = "application/json"
	return headers
}

func (p *Plugin) endpointURL() string {
	baseURL := p.config.EndpointAddrs[randomEndpointIndex(len(p.config.EndpointAddrs))]
	if strings.HasSuffix(baseURL, "/") && strings.HasPrefix(p.config.EndpointURI, "/") {
		return baseURL[:len(baseURL)-1] + p.config.EndpointURI
	}
	return baseURL + p.config.EndpointURI
}

func (p *Plugin) keepalive() bool {
	return p.config.Keepalive == nil || *p.config.Keepalive
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
