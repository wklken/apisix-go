package kafka_logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
	sender kafkaSender
}

const (
	priority = 403
	name     = "kafka-logger"

	originLogKey = "__origin"
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
    "log_format": {
      "type": "object"
    },
    "broker_list": {
      "type": "object",
      "minProperties": 1,
      "patternProperties": {
        ".*": {
          "type": "integer",
          "minimum": 1,
          "maximum": 65535
        }
      }
    },
    "brokers": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "host": {
            "type": "string"
          },
          "port": {
            "type": "integer",
            "minimum": 1,
            "maximum": 65535
          },
          "sasl_config": {
            "type": "object",
            "properties": {
              "mechanism": {
                "type": "string",
                "default": "PLAIN",
                "enum": ["PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512"]
              },
              "user": {
                "type": "string"
              },
              "password": {
                "type": "string"
              }
            },
            "required": ["user", "password"]
          }
        },
        "required": ["host", "port"]
      },
      "uniqueItems": true
    },
    "kafka_topic": {
      "type": "string"
    },
    "producer_type": {
      "type": "string",
      "default": "async",
      "enum": ["async", "sync"]
    },
    "required_acks": {
      "type": "integer",
      "default": 1,
      "enum": [1, -1]
    },
    "key": {
      "type": "string"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 3
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
    "cluster_name": {
      "type": "integer",
      "minimum": 1,
      "default": 1
    },
    "producer_batch_num": {
      "type": "integer",
      "minimum": 1,
      "default": 200
    },
    "producer_batch_size": {
      "type": "integer",
      "minimum": 0,
      "default": 1048576
    },
    "producer_max_buffering": {
      "type": "integer",
      "minimum": 1,
      "default": 50000
    },
    "producer_time_linger": {
      "type": "integer",
      "minimum": 1,
      "default": 1
    },
    "meta_refresh_interval": {
      "type": "integer",
      "minimum": 1,
      "default": 30
    },
    "batch_max_size": {
      "type": "integer",
      "minimum": 1,
      "default": 1000
    },
    "inactive_timeout": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "buffer_duration": {
      "type": "integer",
      "minimum": 1,
      "default": 60
    },
    "retry_delay": {
      "type": "integer",
      "minimum": 0,
      "default": 1
    },
    "max_retry_count": {
      "type": "integer",
      "minimum": 0,
      "default": 0
    },
    "max_pending_entries": {
      "type": "integer",
      "minimum": 1
    }
  },
  "oneOf": [
    {
      "required": ["broker_list", "kafka_topic"]
    },
    {
      "required": ["brokers", "kafka_topic"]
    }
  ]
}
`

type Broker struct {
	Host       string      `json:"host"`
	Port       int         `json:"port"`
	SASLConfig *SASLConfig `json:"sasl_config,omitempty"`
}

type SASLConfig struct {
	Mechanism string `json:"mechanism,omitempty"`
	User      string `json:"user"`
	Password  string `json:"password"`
}

type Config struct {
	MetaFormat   string            `json:"meta_format,omitempty"`
	LogFormat    map[string]string `json:"log_format,omitempty"`
	BrokerList   map[string]int    `json:"broker_list,omitempty"`
	Brokers      []Broker          `json:"brokers,omitempty"`
	KafkaTopic   string            `json:"kafka_topic"`
	ProducerType string            `json:"producer_type,omitempty"`
	RequiredAcks int               `json:"required_acks,omitempty"`
	Key          string            `json:"key,omitempty"`
	Timeout      int               `json:"timeout,omitempty"`

	IncludeReqBody      bool    `json:"include_req_body,omitempty"`
	IncludeReqBodyExpr  [][]any `json:"include_req_body_expr,omitempty"`
	IncludeRespBody     bool    `json:"include_resp_body,omitempty"`
	IncludeRespBodyExpr [][]any `json:"include_resp_body_expr,omitempty"`
	MaxReqBodyBytes     int     `json:"max_req_body_bytes,omitempty"`
	MaxRespBodyBytes    int     `json:"max_resp_body_bytes,omitempty"`

	ClusterName          int `json:"cluster_name,omitempty"`
	ProducerBatchNum     int `json:"producer_batch_num,omitempty"`
	ProducerBatchSize    int `json:"producer_batch_size,omitempty"`
	ProducerMaxBuffering int `json:"producer_max_buffering,omitempty"`
	ProducerTimeLinger   int `json:"producer_time_linger,omitempty"`
	MetaRefreshInterval  int `json:"meta_refresh_interval,omitempty"`

	BatchMaxSize      int `json:"batch_max_size,omitempty"`
	InactiveTimeout   int `json:"inactive_timeout,omitempty"`
	BufferDuration    int `json:"buffer_duration,omitempty"`
	RetryDelay        int `json:"retry_delay,omitempty"`
	MaxRetryCount     int `json:"max_retry_count,omitempty"`
	MaxPendingEntries int `json:"max_pending_entries,omitempty"`
}

type pluginMetadata struct {
	LogFormat         map[string]string `json:"log_format"`
	MaxPendingEntries int               `json:"max_pending_entries,omitempty"`
}

type kafkaMessage struct {
	Topic string
	Key   []byte
	Value []byte
}

type kafkaSender interface {
	Send(ctx context.Context, message kafkaMessage) error
}

type kafkaGoSender struct {
	writer *kafka.Writer
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

	metadata := loadMetadata()
	if len(p.config.LogFormat) > 0 {
		p.LogFormat = p.config.LogFormat
	} else {
		p.LogFormat = metadata.LogFormat
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = metadata.MaxPendingEntries
	}

	if p.sender == nil {
		writer, err := p.newWriter()
		if err != nil {
			return err
		}
		p.sender = &kafkaGoSender{writer: writer}
	}

	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:              "kafka logger",
		BatchMaxSize:      p.config.BatchMaxSize,
		MaxRetryCount:     p.config.MaxRetryCount,
		RetryDelay:        time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:    time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout:   time.Duration(p.config.InactiveTimeout) * time.Second,
		MaxPendingEntries: p.config.MaxPendingEntries,
	}, p.SendBatch)
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if p.config.MetaFormat != "origin" && !p.config.IncludeReqBody && !p.config.IncludeRespBody {
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
		var recorder *kafkaLogResponseRecorder
		if p.config.IncludeRespBody {
			recorder = &kafkaLogResponseRecorder{
				ResponseWriter: w,
				limit:          p.config.MaxRespBodyBytes,
			}
			writer = recorder
		}

		next.ServeHTTP(writer, r)
		if p.config.MetaFormat == "origin" {
			p.Fire(map[string]any{
				originLogKey: buildOriginRequestLog(r, requestBody, p.config.IncludeReqBody),
			})
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

type kafkaLogResponseRecorder struct {
	http.ResponseWriter
	body   bytes.Buffer
	limit  int
	status int
}

func (w *kafkaLogResponseRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *kafkaLogResponseRecorder) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.capture(body)
	return w.ResponseWriter.Write(body)
}

func (w *kafkaLogResponseRecorder) capture(body []byte) {
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
	message, err := encodeKafkaBatch(entries, batchMaxSize)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal kafka log message: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.config.Timeout)*time.Second)
	defer cancel()

	err = p.sender.Send(ctx, kafkaMessage{
		Topic: p.config.KafkaTopic,
		Key:   []byte(p.config.Key),
		Value: message,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to send data to Kafka topic %s: %w", p.config.KafkaTopic, err)
	}
	return 0, nil
}

func encodeKafkaBatch(entries []map[string]any, batchMaxSize int) ([]byte, error) {
	if rawEntries, ok := originLogEntries(entries); ok {
		if batchMaxSize == 1 && len(rawEntries) == 1 {
			return []byte(rawEntries[0]), nil
		}
		return json.Marshal(rawEntries)
	}
	if batchMaxSize == 1 && len(entries) == 1 {
		return json.Marshal(entries[0])
	}
	return json.Marshal(entries)
}

func originLogEntries(entries []map[string]any) ([]string, bool) {
	if len(entries) == 0 {
		return nil, false
	}
	rawEntries := make([]string, 0, len(entries))
	for _, entry := range entries {
		raw, ok := entry[originLogKey].(string)
		if !ok {
			return nil, false
		}
		rawEntries = append(rawEntries, raw)
	}
	return rawEntries, true
}

func buildOriginRequestLog(r *http.Request, requestBody string, includeReqBody bool) string {
	var b strings.Builder
	requestURI := r.URL.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	_, _ = fmt.Fprintf(&b, "%s %s %s\r\n", r.Method, requestURI, r.Proto)

	headerNames := make([]string, 0, len(r.Header))
	for name := range r.Header {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		for _, value := range r.Header.Values(name) {
			_, _ = fmt.Fprintf(&b, "%s: %s\r\n", name, value)
		}
	}

	b.WriteString("\r\n")
	if includeReqBody {
		b.WriteString(requestBody)
	}
	return b.String()
}

func (p *Plugin) applyDefaults() {
	if p.config.MetaFormat == "" {
		p.config.MetaFormat = "default"
	}
	if p.config.ProducerType == "" {
		p.config.ProducerType = "async"
	}
	if p.config.RequiredAcks == 0 {
		p.config.RequiredAcks = 1
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.ClusterName == 0 {
		p.config.ClusterName = 1
	}
	if p.config.ProducerBatchNum == 0 {
		p.config.ProducerBatchNum = 200
	}
	if p.config.ProducerBatchSize == 0 {
		p.config.ProducerBatchSize = 1048576
	}
	if p.config.ProducerMaxBuffering == 0 {
		p.config.ProducerMaxBuffering = 50000
	}
	if p.config.ProducerTimeLinger == 0 {
		p.config.ProducerTimeLinger = 1
	}
	if p.config.MetaRefreshInterval == 0 {
		p.config.MetaRefreshInterval = 30
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
	if p.config.MaxReqBodyBytes == 0 {
		p.config.MaxReqBodyBytes = base.MAX_REQ_BODY
	}
	if p.config.MaxRespBodyBytes == 0 {
		p.config.MaxRespBodyBytes = base.MAX_RESP_BODY
	}
}

func (p *Plugin) newWriter() (*kafka.Writer, error) {
	mechanism, err := p.saslMechanism()
	if err != nil {
		return nil, err
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(p.brokerAddresses()...),
		Topic:        p.config.KafkaTopic,
		RequiredAcks: kafka.RequiredAcks(p.config.RequiredAcks),
		Async:        p.config.ProducerType == "async",
		BatchSize:    p.config.ProducerBatchNum,
		BatchBytes:   int64(p.config.ProducerBatchSize),
		BatchTimeout: time.Duration(p.config.ProducerTimeLinger) * time.Millisecond,
		WriteTimeout: time.Duration(p.config.Timeout) * time.Second,
		ReadTimeout:  time.Duration(p.config.Timeout) * time.Second,
	}
	if mechanism != nil {
		writer.Transport = &kafka.Transport{
			DialTimeout: time.Duration(p.config.Timeout) * time.Second,
			SASL:        mechanism,
		}
	}

	return writer, nil
}

func (p *Plugin) saslMechanism() (sasl.Mechanism, error) {
	for _, broker := range p.config.Brokers {
		if broker.SASLConfig == nil {
			continue
		}

		mechanism := strings.ToUpper(broker.SASLConfig.Mechanism)
		if mechanism == "" {
			mechanism = "PLAIN"
		}

		switch mechanism {
		case "PLAIN":
			return plain.Mechanism{
				Username: broker.SASLConfig.User,
				Password: broker.SASLConfig.Password,
			}, nil
		case "SCRAM-SHA-256":
			return scram.Mechanism(scram.SHA256, broker.SASLConfig.User, broker.SASLConfig.Password)
		case "SCRAM-SHA-512":
			return scram.Mechanism(scram.SHA512, broker.SASLConfig.User, broker.SASLConfig.Password)
		default:
			return nil, fmt.Errorf("unsupported Kafka SASL mechanism %q", broker.SASLConfig.Mechanism)
		}
	}

	return nil, nil
}

func (p *Plugin) brokerAddresses() []string {
	addresses := make([]string, 0, len(p.config.Brokers)+len(p.config.BrokerList))
	for _, broker := range p.config.Brokers {
		addresses = append(addresses, net.JoinHostPort(broker.Host, fmt.Sprint(broker.Port)))
	}

	keys := make([]string, 0, len(p.config.BrokerList))
	for host := range p.config.BrokerList {
		keys = append(keys, host)
	}
	sort.Strings(keys)
	for _, host := range keys {
		addresses = append(addresses, net.JoinHostPort(host, fmt.Sprint(p.config.BrokerList[host])))
	}

	return addresses
}

func (s *kafkaGoSender) Send(ctx context.Context, message kafkaMessage) error {
	return s.writer.WriteMessages(ctx, kafka.Message{
		Topic: message.Topic,
		Key:   message.Key,
		Value: message.Value,
	})
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
