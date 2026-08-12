package kafka_logger

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

type Plugin struct {
	base.BaseLoggerPlugin
	config Config
	sender kafkaSender
	stop   sync.Once
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
    "api_version": {
      "type": "integer",
      "default": 1,
      "enum": [0, 1, 2]
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
	APIVersion           int `json:"api_version,omitempty"`

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
	p.applyDefaults()
	if err := base.PrepareExprRegexps(
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	); err != nil {
		return err
	}
	if err := validateBodyExpression("include_req_body_expr", p.config.IncludeReqBodyExpr); err != nil {
		return err
	}
	if err := validateBodyExpression("include_resp_body_expr", p.config.IncludeRespBodyExpr); err != nil {
		return err
	}
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
	p.SetLogCapturePolicy(
		p.config.IncludeReqBody, p.config.IncludeRespBody,
		p.config.MaxReqBodyBytes, p.config.MaxRespBodyBytes,
		p.config.IncludeReqBodyExpr, p.config.IncludeRespBodyExpr,
	)

	if p.sender == nil {
		writer, err := p.newWriter()
		if err != nil {
			return err
		}
		p.sender = &kafkaGoSender{writer: writer}
	}

	p.BatchProcessor = base.NewBatchProcessor("kafka logger", base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
		PluginID:           name,
	}, p.RouteID, p.ServerAddr, p.SendBatch)
	return nil
}

func validateBodyExpression(field string, expression [][]any) error {
	for _, condition := range expression {
		if len(condition) != 3 {
			return fmt.Errorf("failed to validate the %q expression: each condition must contain 3 items", field)
		}
		operator, ok := condition[1].(string)
		if !ok {
			return fmt.Errorf("failed to validate the %q expression: operator must be a string", field)
		}
		switch operator {
		case "==", "!=", ">", ">=", "<", "<=", "~", "!~", "in":
		default:
			return fmt.Errorf("failed to validate the %q expression: invalid operator %q", field, operator)
		}
	}
	return nil
}

func (p *Plugin) Stop() {
	p.stop.Do(func() {
		p.StopWithCleanup(func() {
			if closer, ok := p.sender.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		})
	})
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
		if p.config.MetaFormat == "origin" {
			_ = p.Fire(map[string]any{
				originLogKey: buildOriginRequestLog(r, requestBody, p.config.IncludeReqBody),
			})
			return
		}

		status := metrics.Code
		var logFields map[string]any
		if len(p.LogFormat) > 0 {
			logFields = apisixlog.GetFields(r, p.LogFormat)
		} else {
			logFields = p.defaultLogFields(r, metrics)
		}
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

func (p *Plugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if p.config.MetaFormat == "origin" {
		body := ""
		if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
			body = base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes)
		}
		return p.EnqueueLog(map[string]any{originLogKey: kafkaSnapshotOrigin(snapshot, body)})
	}
	var fields map[string]any
	if len(p.LogFormat) > 0 {
		fields = base.GetFieldsFromSnapshot(snapshot, p.LogFormat)
	} else {
		fields = kafkaSnapshotDefaultFields(p, snapshot)
	}
	if p.config.IncludeReqBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeReqBodyExpr) {
		if body := base.SnapshotRequestBody(snapshot, p.config.MaxReqBodyBytes); body != "" {
			base.NestedLogMap(fields, "request")["body"] = body
		}
	}
	if p.config.IncludeRespBody && base.SnapshotExpressionMatches(snapshot, p.config.IncludeRespBodyExpr) {
		if body := base.SnapshotResponseBody(snapshot, p.config.MaxRespBodyBytes); body != "" {
			base.NestedLogMap(fields, "response")["body"] = body
		}
	}
	return p.EnqueueLog(fields)
}

func kafkaSnapshotDefaultFields(p *Plugin, snapshot base.LogSnapshot) map[string]any {
	host := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_ip"))
	port := fmt.Sprint(base.SnapshotValue(snapshot, "$balancer_port"))
	upstream := host
	if host != "" && port != "" {
		upstream = net.JoinHostPort(host, port)
	}
	routeID := p.RouteID
	if routeID == "" {
		routeID = fmt.Sprint(base.SnapshotValue(snapshot, "$route_id"))
	}
	return map[string]any{
		"route_id":   routeID,
		"service_id": base.SnapshotValue(snapshot, "$service_id"),
		"client_ip":  base.RemoteIP(snapshot.Request.RemoteAddr),
		"upstream":   upstream,
		"request":    map[string]any{"method": snapshot.Request.Method, "uri": snapshotURI(snapshot)},
		"response":   map[string]any{"status": snapshot.Outcome.Status, "size": snapshot.Outcome.Bytes},
	}
}

func kafkaSnapshotOrigin(snapshot base.LogSnapshot, body string) string {
	var b strings.Builder
	proto := snapshot.Request.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	fmt.Fprintf(&b, "%s %s %s\r\n", snapshot.Request.Method, snapshotURI(snapshot), proto)
	names := make([]string, 0, len(snapshot.Request.Header))
	for name := range snapshot.Request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range snapshot.Request.Header.Values(name) {
			fmt.Fprintf(&b, "%s: %s\r\n", name, value)
		}
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

func snapshotURI(snapshot base.LogSnapshot) string {
	if snapshot.Request.URI != "" {
		return snapshot.Request.URI
	}
	if snapshot.Request.URL != "" {
		if parsed, err := url.Parse(snapshot.Request.URL); err == nil && parsed.RequestURI() != "" {
			return parsed.RequestURI()
		}
	}
	return "/"
}

func (p *Plugin) defaultLogFields(r *http.Request, metrics httpsnoop.Metrics) map[string]any {
	upstreamHost := base.RequestVar(r, "$balancer_ip", metrics.Code)
	upstreamPort := base.RequestVar(r, "$balancer_port", metrics.Code)
	upstream := upstreamHost
	if upstreamHost != "" && upstreamPort != "" {
		upstream = net.JoinHostPort(upstreamHost, upstreamPort)
	}
	return map[string]any{
		"route_id":   p.RouteID,
		"service_id": base.RequestVar(r, "$service_id", metrics.Code),
		"client_ip":  base.RemoteIP(r.RemoteAddr),
		"upstream":   upstream,
		"request": map[string]any{
			"method": r.Method,
			"uri":    r.URL.RequestURI(),
		},
		"response": map[string]any{
			"status": metrics.Code,
			"size":   metrics.Written,
		},
	}
}

func (p *Plugin) Send(log map[string]any) {
	if _, err := p.SendBatch(context.Background(), []map[string]any{log}, 1); err != nil {
		logger.Errorf("%s", err)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
	message, err := encodeKafkaBatch(entries, batchMaxSize)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal kafka log message: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.config.Timeout)*time.Second)
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
	return base.EncodeLogBatch(entries, batchMaxSize, originLogKey)
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
