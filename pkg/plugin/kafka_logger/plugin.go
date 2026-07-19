package kafka_logger

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
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
	p.applyDefaults()
	if err := p.resolveSecrets(); err != nil {
		return err
	}

	metadata := base.LoadPluginMetadata[pluginMetadata](name)
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
		RouteID:           p.RouteID,
		ServerAddr:        p.ServerAddr,
	}, p.SendBatch)
	return nil
}

func (p *Plugin) resolveSecrets() error {
	keyring, enabled := data_encryption.Keyring()
	resolver := data_encryption.NewResolver(enabled, keyring)
	for i := range p.config.Brokers {
		config := p.config.Brokers[i].SASLConfig
		if config == nil {
			continue
		}
		resolved, err := resolver.Resolve(config.Password)
		if err != nil {
			return fmt.Errorf("kafka-logger brokers[%d].sasl_config.password: %w", i, err)
		}
		config.Password = resolved
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	if p.config.MetaFormat != "origin" && !p.config.IncludeReqBody && !p.config.IncludeRespBody {
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
		if p.config.MetaFormat == "origin" {
			_ = p.Fire(map[string]any{
				originLogKey: buildOriginRequestLog(r, requestBody, p.config.IncludeReqBody),
			})
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
