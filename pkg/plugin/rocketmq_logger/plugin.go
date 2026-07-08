package rocketmq_logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
	sender rocketmqSender
}

const (
	priority = 402
	name     = "rocketmq-logger"
)

const schema = `
{
  "type": "object",
  "properties": {
    "meta_format": {
      "type": "string",
      "default": "default",
      "enum": ["default", "origin"]
    },
    "nameserver_list": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "topic": {
      "type": "string"
    },
    "key": {
      "type": "string"
    },
    "tag": {
      "type": "string"
    },
    "log_format": {
      "type": "object"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
    },
    "use_tls": {
      "type": "boolean",
      "default": false
    },
    "access_key": {
      "type": "string",
      "default": ""
    },
    "secret_key": {
      "type": "string",
      "default": ""
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
    }
  },
  "required": ["nameserver_list", "topic"]
}
`

type Config struct {
	MetaFormat     string            `json:"meta_format,omitempty"`
	NameServerList []string          `json:"nameserver_list"`
	Topic          string            `json:"topic"`
	Key            string            `json:"key,omitempty"`
	Tag            string            `json:"tag,omitempty"`
	LogFormat      map[string]string `json:"log_format,omitempty"`
	Timeout        int               `json:"timeout,omitempty"`
	UseTLS         bool              `json:"use_tls,omitempty"`
	AccessKey      string            `json:"access_key,omitempty"`
	SecretKey      string            `json:"secret_key,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`
}

type pluginMetadata struct {
	LogFormat map[string]string `json:"log_format"`
}

type rocketmqMessage struct {
	Topic string
	Key   string
	Tag   string
	Body  []byte
}

type rocketmqSender interface {
	Send(ctx context.Context, message rocketmqMessage) error
}

type rocketmqClientSender struct {
	producer rocketmq.Producer
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

	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = loadMetadataLogFormat()
	}

	if p.sender == nil {
		sender, err := p.newSender()
		if err != nil {
			return err
		}
		p.sender = sender
	}

	p.Consume()
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
		var recorder *rocketMQLogResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &rocketMQLogResponseRecorder{
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

type rocketMQLogResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *rocketMQLogResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *rocketMQLogResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *rocketMQLogResponseRecorder) capture(body []byte) {
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
	message, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal rocketmq log message: %s", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.config.Timeout)*time.Second)
	defer cancel()

	err = p.sender.Send(ctx, rocketmqMessage{
		Topic: p.config.Topic,
		Key:   p.config.Key,
		Tag:   p.config.Tag,
		Body:  message,
	})
	if err != nil {
		logger.Errorf("failed to send data to RocketMQ topic %s: %s", p.config.Topic, err)
	}
}

func (p *Plugin) applyDefaults() {
	if p.config.MetaFormat == "" {
		p.config.MetaFormat = "default"
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
}

func (p *Plugin) newSender() (rocketmqSender, error) {
	options := []producer.Option{
		producer.WithNameServer(p.config.NameServerList),
		producer.WithSendMsgTimeout(time.Duration(p.config.Timeout) * time.Second),
	}
	if p.config.AccessKey != "" {
		options = append(options, producer.WithCredentials(primitive.Credentials{
			AccessKey: p.config.AccessKey,
			SecretKey: p.config.SecretKey,
		}))
	}

	prod, err := rocketmq.NewProducer(options...)
	if err != nil {
		return nil, err
	}
	if err := prod.Start(); err != nil {
		return nil, err
	}

	return &rocketmqClientSender{producer: prod}, nil
}

func (s *rocketmqClientSender) Send(ctx context.Context, message rocketmqMessage) error {
	msg := primitive.NewMessage(message.Topic, message.Body)
	if message.Tag != "" {
		msg.WithTag(message.Tag)
	}
	if message.Key != "" {
		msg.WithKeys([]string{message.Key})
	}

	_, err := s.producer.SendSync(ctx, msg)
	return err
}

func loadMetadataLogFormat() (format map[string]string) {
	defer func() {
		if recover() != nil {
			format = nil
		}
	}()

	var metadata pluginMetadata
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return nil
	}
	return metadata.LogFormat
}
