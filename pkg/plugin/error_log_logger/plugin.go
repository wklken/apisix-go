package error_log_logger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client         *http.Client
	kafkaSender    kafkaSender
	BatchProcessor *logger_batch.Processor
	observerStop   func()
	stopOnce       sync.Once

	observerLifecycleMu sync.Mutex
	observerStopped     bool

	clickhousePassword         *secret.Value
	clickhousePasswordLegacy   *string
	kafkaPasswords             []indexedSecret
	kafkaPasswordsLegacy       []indexedLegacySecret
	metadataClickhousePassword *metadataSecret
	metadataKafkaPasswords     []indexedMetadataSecret
	metadataSelected           bool
	secretsMaterialized        bool

	dialTCP func(ctx context.Context, network, address string, timeout time.Duration, tlsConfig *tls.Config, useTLS bool) (net.Conn, error)
}

const (
	priority = 1091
	name     = "error-log-logger"
)

const schema = `
{
  "type": "object",
  "properties": {
    "id": {"type": "string", "const": "error-log-logger"},
    "tcp": {
      "type": "object",
      "required": ["host", "port"],
      "properties": {
        "host": {"type": "string", "minLength": 1},
        "port": {"type": "integer", "minimum": 1, "maximum": 65535},
        "tls": {"type": "boolean"},
        "tls_server_name": {"type": "string"}
      },
      "additionalProperties": false
    },
    "skywalking": {
      "type": "object",
      "properties": {
        "endpoint_addr": {"type": "string", "minLength": 1},
        "service_name": {"type": "string", "minLength": 1},
        "service_instance_name": {"type": "string", "minLength": 1}
      },
      "additionalProperties": false
    },
    "clickhouse": {
      "type": "object",
      "required": ["user", "password", "database", "logtable", "endpoint_addr"],
      "properties": {
        "endpoint_addr": {"type": "string", "minLength": 1},
        "user": {"type": "string", "minLength": 1},
        "password": {"type": "string"},
        "database": {"type": "string", "minLength": 1},
        "logtable": {"type": "string", "minLength": 1}
      },
      "additionalProperties": false
    },
    "kafka": {
      "type": "object",
      "required": ["brokers", "kafka_topic"],
      "properties": {
        "brokers": {
          "type": "array",
          "minItems": 1,
          "items": {
            "type": "object",
            "required": ["host", "port"],
            "properties": {
              "host": {"type": "string", "minLength": 1},
              "port": {"type": "integer", "minimum": 1, "maximum": 65535},
              "sasl_config": {
                "type": "object",
                "required": ["user", "password"],
                "properties": {
                  "mechanism": {"type": "string", "enum": ["PLAIN", "plain"]},
                  "user": {"type": "string", "minLength": 1},
                  "password": {"type": "string"}
                },
                "additionalProperties": false
              }
            },
            "additionalProperties": false
          }
        },
        "kafka_topic": {"type": "string", "minLength": 1},
        "producer_type": {"type": "string", "enum": ["sync", "async"]},
        "required_acks": {"type": "integer", "enum": [-1, 0, 1]},
        "key": {"type": "string"},
        "cluster_name": {"type": "integer", "minimum": 1},
        "meta_refresh_interval": {"type": "integer", "minimum": 1}
      },
      "additionalProperties": false
    },
    "host": {"type": "string", "minLength": 1},
    "port": {"type": "integer", "minimum": 1, "maximum": 65535},
    "tls": {"type": "boolean"},
    "tls_server_name": {"type": "string"},
    "name": {"type": "string", "minLength": 1},
    "level": {
      "type": "string",
      "enum": ["STDERR", "EMERG", "ALERT", "CRIT", "ERR", "ERROR", "WARN", "NOTICE", "INFO", "DEBUG",
               "stderr", "emerg", "alert", "crit", "err", "error", "warn", "notice", "info", "debug"]
    },
    "timeout": {"type": "integer", "minimum": 1},
    "keepalive": {"type": "integer", "minimum": 1},
    "batch_max_size": {"type": "integer", "minimum": 1},
    "max_retry_count": {"type": "integer", "minimum": 0},
    "retry_delay": {"type": "integer", "minimum": 0},
    "buffer_duration": {"type": "integer", "minimum": 1},
    "inactive_timeout": {"type": "integer", "minimum": 1},
    "max_pending_entries": {"type": "integer", "minimum": 1, "default": 10000}
  },
  "anyOf": [
    {"maxProperties": 0},
    {"required": ["tcp"]},
    {"required": ["skywalking"]},
    {"required": ["clickhouse"]},
    {"required": ["kafka"]},
    {"required": ["host", "port"]}
  ],
  "additionalProperties": false
}
`

type Config struct {
	TCP        *TCPConfig        `json:"tcp,omitempty"`
	Skywalking *SkywalkingConfig `json:"skywalking,omitempty"`
	Clickhouse *ClickHouseConfig `json:"clickhouse,omitempty"`
	Kafka      *KafkaConfig      `json:"kafka,omitempty"`

	Host          string `json:"host,omitempty"`
	Port          int    `json:"port,omitempty"`
	TLS           bool   `json:"tls,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`

	Name              string `json:"name,omitempty"`
	Level             string `json:"level,omitempty"`
	Timeout           int    `json:"timeout,omitempty"`
	Keepalive         int    `json:"keepalive,omitempty"`
	BatchMaxSize      int    `json:"batch_max_size,omitempty"`
	MaxRetryCount     int    `json:"max_retry_count,omitempty"`
	RetryDelay        int    `json:"retry_delay,omitempty"`
	BufferDuration    int    `json:"buffer_duration,omitempty"`
	InactiveTimeout   int    `json:"inactive_timeout,omitempty"`
	MaxPendingEntries int    `json:"max_pending_entries,omitempty"`
}

type TCPConfig struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	TLS           bool   `json:"tls,omitempty"`
	TLSServerName string `json:"tls_server_name,omitempty"`
}

type SkywalkingConfig struct {
	EndpointAddr        string `json:"endpoint_addr,omitempty"`
	ServiceName         string `json:"service_name,omitempty"`
	ServiceInstanceName string `json:"service_instance_name,omitempty"`
}

type ClickHouseConfig struct {
	EndpointAddr string `json:"endpoint_addr,omitempty"`
	User         string `json:"user,omitempty"`
	Password     string `json:"password,omitempty"`
	Database     string `json:"database,omitempty"`
	LogTable     string `json:"logtable,omitempty"`
}

type KafkaConfig struct {
	Brokers             []KafkaBroker `json:"brokers,omitempty"`
	KafkaTopic          string        `json:"kafka_topic"`
	ProducerType        string        `json:"producer_type,omitempty"`
	RequiredAcks        int           `json:"required_acks,omitempty"`
	Key                 string        `json:"key,omitempty"`
	ClusterName         int           `json:"cluster_name,omitempty"`
	MetaRefreshInterval int           `json:"meta_refresh_interval,omitempty"`
}

type KafkaBroker struct {
	Host       string      `json:"host"`
	Port       int         `json:"port"`
	SASLConfig *SASLConfig `json:"sasl_config,omitempty"`
}

type SASLConfig struct {
	Mechanism string `json:"mechanism,omitempty"`
	User      string `json:"user"`
	Password  string `json:"password"`
}

type kafkaMessage struct {
	Topic string
	Key   []byte
	Value []byte
}

type kafkaSender interface {
	Send(ctx context.Context, message kafkaMessage) error
	Close() error
}

type kafkaGoSender struct {
	writer *kafka.Writer
}

type indexedSecret struct {
	index int
	value secret.Value
}

type indexedLegacySecret struct {
	index int
	value string
}

type metadataSecret struct {
	plaintext  string
	descriptor secret.Descriptor
}

type indexedMetadataSecret struct {
	index int
	value metadataSecret
}

var (
	errMetadataConfigInvalid          = errors.New("error-log-logger metadata configuration is invalid")
	errMetadataSecretInstallationFail = errors.New("error-log-logger metadata secret installation failed")
	errObserverTaskRegistryRequired   = errors.New("error-log-logger observer task registry is required")
	errObserverTaskRegistration       = errors.New("error-log-logger observer task registration failed")
	errObserverLifecycleStopped       = errors.New("error-log-logger observer lifecycle is stopped")
)

var levelPattern = regexp.MustCompile(`\[(stderr|emerg|alert|crit|err|error|warn|notice|info|debug)\]`)

var levelOrder = map[string]int{
	"STDERR": 0,
	"EMERG":  1,
	"ALERT":  2,
	"CRIT":   3,
	"ERR":    4,
	"ERROR":  4,
	"WARN":   5,
	"NOTICE": 6,
	"INFO":   7,
	"DEBUG":  8,
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	metadataConfig, hasMetadata, err := p.decodeMetadataConfig()
	if err != nil {
		return err
	}
	if !hasExplicitConfig(p.config) && hasMetadata {
		p.config = metadataConfig
		p.metadataSelected = true
		if err := p.installMetadataSecrets(); err != nil {
			return err
		}
	}
	p.applyDefaults()
	p.client = &http.Client{Timeout: time.Duration(p.config.Timeout) * time.Second}
	if p.config.Kafka != nil && p.kafkaSender == nil {
		writer, err := p.newKafkaWriter()
		if err != nil {
			return err
		}
		p.kafkaSender = &kafkaGoSender{writer: writer}
	}
	p.BatchProcessor = base.NewBatchProcessor(p.config.Name, base.BatchDefaults{
		BatchMaxSize:       p.config.BatchMaxSize,
		MaxRetryCount:      p.config.MaxRetryCount,
		RetryDelaySec:      p.config.RetryDelay,
		BufferDurationSec:  p.config.BufferDuration,
		InactiveTimeoutSec: p.config.InactiveTimeout,
		MaxPendingEntries:  p.config.MaxPendingEntries,
		PluginID:           name,
	}, "", "", p.SendBatch)

	return nil
}

func (p *Plugin) decodeMetadataConfig() (Config, bool, error) {
	var metadata map[string]any
	found, err := p.MetadataView().Decode(name, &metadata)
	if err != nil {
		return Config{}, false, errMetadataConfigInvalid
	}
	if !found {
		return Config{}, false, nil
	}
	if err := util.Validate(metadata, p.MetadataSchema); err != nil {
		return Config{}, false, errMetadataConfigInvalid
	}
	var config Config
	if err := util.Parse(metadata, &config); err != nil {
		return Config{}, false, errMetadataConfigInvalid
	}
	return config, true, nil
}

func hasExplicitConfig(config Config) bool {
	return config.TCP != nil || config.Skywalking != nil || config.Clickhouse != nil ||
		config.Kafka != nil || config.Host != "" || config.Port != 0
}

func (p *Plugin) installMetadataSecrets() error {
	var clickhousePassword *metadataSecret
	metadataKafkaPasswords := make([]indexedMetadataSecret, 0)

	if p.config.Clickhouse != nil {
		value, err := newMetadataSecret(p.config.Clickhouse.Password)
		if err != nil {
			return errMetadataSecretInstallationFail
		}
		clickhousePassword = &value
	}
	if p.config.Kafka != nil {
		for index, broker := range p.config.Kafka.Brokers {
			if broker.SASLConfig == nil {
				continue
			}
			value, err := newMetadataSecret(broker.SASLConfig.Password)
			if err != nil {
				return errMetadataSecretInstallationFail
			}
			metadataKafkaPasswords = append(metadataKafkaPasswords, indexedMetadataSecret{
				index: index,
				value: value,
			})
		}
	}

	if clickhousePassword != nil {
		p.config.Clickhouse.Password = clickhousePassword.descriptor.String()
	}
	for _, item := range metadataKafkaPasswords {
		p.config.Kafka.Brokers[item.index].SASLConfig.Password = item.value.descriptor.String()
	}
	p.metadataClickhousePassword = clickhousePassword
	p.metadataKafkaPasswords = metadataKafkaPasswords
	return nil
}

func newMetadataSecret(plaintext string) (metadataSecret, error) {
	digest := sha256.Sum256([]byte(plaintext))
	descriptor, err := secret.NewDescriptor(capability.SecretPluginMetadata, digest)
	if err != nil {
		return metadataSecret{}, err
	}
	return metadataSecret{plaintext: plaintext, descriptor: descriptor}, nil
}

// MaterializeSecrets is the transitional Builder path. It is deliberately
// separate from MaterializeScopedSecrets so a legacy process-global resolver
// cannot become the scoped attempt's authority by accident.
func (p *Plugin) MaterializeSecrets() error {
	if p.secretsMaterialized {
		return nil
	}
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return fmt.Errorf("%s: %w", name, secret.ErrCredentialUnavailable)
	}

	var clickhousePassword *string
	var clickhouseDescriptor string
	if p.config.Clickhouse != nil {
		resolved, err := resolver.ResolveForContext(
			p.config.Clickhouse.Password,
			"error-log-logger.clickhouse.password",
		)
		if err != nil {
			return fmt.Errorf("%s clickhouse.password: %w", name, secret.ErrCredentialUnavailable)
		}
		descriptor, err := descriptorForLegacySecret(resolved)
		if err != nil {
			return fmt.Errorf("%s clickhouse.password: %w", name, secret.ErrCredentialUnavailable)
		}
		clickhousePassword = &resolved
		clickhouseDescriptor = descriptor.String()
	}

	legacyKafkaPasswords := make([]indexedLegacySecret, 0)
	kafkaDescriptors := make([]struct {
		index      int
		descriptor string
	}, 0)
	if p.config.Kafka != nil {
		for i := range p.config.Kafka.Brokers {
			config := p.config.Kafka.Brokers[i].SASLConfig
			if config == nil {
				continue
			}
			resolved, err := resolver.ResolveForContext(
				config.Password,
				"error-log-logger.kafka.brokers.*.sasl_config.password",
			)
			if err != nil {
				return fmt.Errorf("%s kafka.brokers.*.sasl_config.password: %w", name, secret.ErrCredentialUnavailable)
			}
			descriptor, err := descriptorForLegacySecret(resolved)
			if err != nil {
				return fmt.Errorf("%s kafka.brokers.*.sasl_config.password: %w", name, secret.ErrCredentialUnavailable)
			}
			legacyKafkaPasswords = append(legacyKafkaPasswords, indexedLegacySecret{index: i, value: resolved})
			kafkaDescriptors = append(kafkaDescriptors, struct {
				index      int
				descriptor string
			}{index: i, descriptor: descriptor.String()})
		}
	}

	if p.config.Clickhouse != nil {
		p.config.Clickhouse.Password = clickhouseDescriptor
	}
	if p.config.Kafka != nil {
		for _, item := range kafkaDescriptors {
			p.config.Kafka.Brokers[item.index].SASLConfig.Password = item.descriptor
		}
	}
	p.clickhousePasswordLegacy = clickhousePassword
	p.kafkaPasswordsLegacy = legacyKafkaPasswords
	p.secretsMaterialized = true
	return nil
}

// MaterializeScopedSecrets admits only plugin-config declarations. Metadata
// declarations intentionally remain owned by the later M2 lifecycle work.
func (p *Plugin) MaterializeScopedSecrets(ctx context.Context, access base.ScopedSecretAccess) error {
	if p.metadataSelected {
		return nil
	}
	if p.secretsMaterialized {
		return nil
	}

	var clickhousePassword *secret.Value
	var clickhouseDescriptor string
	if p.config.Clickhouse != nil {
		value, err := access.Materialize(ctx, "clickhouse.password", p.config.Clickhouse.Password)
		if err != nil {
			return fmt.Errorf("%s clickhouse.password: %w", name, secret.ErrCredentialUnavailable)
		}
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil {
			return fmt.Errorf("%s clickhouse.password: %w", name, secret.ErrCredentialUnavailable)
		}
		clickhousePassword = &value
		clickhouseDescriptor = descriptor.String()
	}

	kafkaPasswords := make([]indexedSecret, 0)
	kafkaDescriptors := make([]struct {
		index      int
		descriptor string
	}, 0)
	if p.config.Kafka != nil {
		for i := range p.config.Kafka.Brokers {
			config := p.config.Kafka.Brokers[i].SASLConfig
			if config == nil {
				continue
			}
			value, err := access.Materialize(
				ctx,
				"kafka.brokers.*.sasl_config.password",
				config.Password,
			)
			if err != nil {
				return fmt.Errorf("%s kafka.brokers.*.sasl_config.password: %w", name, secret.ErrCredentialUnavailable)
			}
			descriptor, err := value.Descriptor(capability.SecretPluginConfig)
			if err != nil {
				return fmt.Errorf("%s kafka.brokers.*.sasl_config.password: %w", name, secret.ErrCredentialUnavailable)
			}
			kafkaPasswords = append(kafkaPasswords, indexedSecret{index: i, value: value})
			kafkaDescriptors = append(kafkaDescriptors, struct {
				index      int
				descriptor string
			}{index: i, descriptor: descriptor.String()})
		}
	}

	if p.config.Clickhouse != nil {
		p.config.Clickhouse.Password = clickhouseDescriptor
	}
	if p.config.Kafka != nil {
		for _, item := range kafkaDescriptors {
			p.config.Kafka.Brokers[item.index].SASLConfig.Password = item.descriptor
		}
	}
	p.clickhousePassword = clickhousePassword
	p.kafkaPasswords = kafkaPasswords
	p.secretsMaterialized = true
	return nil
}

func descriptorForLegacySecret(value string) (secret.Descriptor, error) {
	digest := sha256.Sum256([]byte(value))
	return secret.NewDescriptor(capability.SecretPluginConfig, digest)
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *Plugin) StartObserving() {
	p.observerLifecycleMu.Lock()
	defer p.observerLifecycleMu.Unlock()
	if p.observerStopped {
		return
	}
	p.observerStop = p.installObserver()
}

func (p *Plugin) StartObservingWithTasks(tasks *runtime.TaskRegistry) error {
	if tasks == nil {
		return errObserverTaskRegistryRequired
	}
	p.observerLifecycleMu.Lock()
	defer p.observerLifecycleMu.Unlock()
	if p.observerStopped {
		return errObserverLifecycleStopped
	}

	ready := make(chan struct{})
	if err := tasks.Go(
		runtime.TaskSpec{Owner: "plugin/error-log-logger/observer", Criticality: runtime.TaskPlugin},
		func(ctx context.Context) error {
			select {
			case <-ready:
			case <-ctx.Done():
				<-ready
			}
			if ctx.Err() == nil {
				<-ctx.Done()
			}
			p.Stop()
			return nil
		},
	); err != nil {
		return errObserverTaskRegistration
	}

	stop := p.installObserver()
	p.observerStop = stop
	close(ready)
	return nil
}

func (p *Plugin) installObserver() func() {
	processorName := strings.ToLower(p.config.Name)
	return logger.ReplaceObserver(name, func(entry logger.Entry) {
		message := strings.ToLower(entry.Message)
		if strings.Contains(message, "logger batch processor ["+processorName+"]") {
			return
		}
		threshold := levelOrder[p.config.Level]
		if level, ok := levelOrder[strings.ToUpper(entry.Level)]; ok && level > threshold {
			return
		}
		if p.BatchProcessor != nil {
			_ = p.BatchProcessor.Push(map[string]any{"message": entry.Line})
		}
	})
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		p.observerLifecycleMu.Lock()
		p.observerStopped = true
		stopObserver := p.observerStop
		p.observerStop = nil
		p.observerLifecycleMu.Unlock()

		if stopObserver != nil {
			stopObserver()
		}
		cleanup := func() {
			if p.kafkaSender != nil {
				sender := p.kafkaSender
				if err := sender.Close(); err != nil {
					logger.Errorf("failed to close error-log-logger kafka writer: %s", err)
				}
				p.kafkaSender = nil
			}
			p.clickhousePassword = nil
			p.clickhousePasswordLegacy = nil
			p.kafkaPasswords = nil
			p.kafkaPasswordsLegacy = nil
			p.metadataClickhousePassword = nil
			p.metadataKafkaPasswords = nil
		}
		if p.BatchProcessor != nil {
			p.BatchProcessor.StopWithCleanup(cleanup)
		} else {
			cleanup()
		}
	})
}

func (p *Plugin) SendLogs(ctx context.Context, lines []string) error {
	filtered := p.filterLogs(lines)
	if len(filtered) == 0 {
		return nil
	}

	switch {
	case p.config.Skywalking != nil:
		return p.sendToSkywalking(ctx, filtered)
	case p.config.Clickhouse != nil:
		return p.sendToClickHouse(ctx, filtered)
	case p.config.Kafka != nil:
		return p.sendToKafka(ctx, filtered)
	default:
		return p.sendToTCP(ctx, filtered)
	}
}

func (p *Plugin) SendBatch(ctx context.Context, entries []map[string]any, _ int) (int, error) {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line, err := logLine(entry)
		if err != nil {
			return 0, err
		}
		lines = append(lines, line)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return 0, p.SendLogs(ctx, lines)
}

func (p *Plugin) applyDefaults() {
	if p.config.Name == "" {
		p.config.Name = name
	}
	if p.config.Level == "" {
		p.config.Level = "WARN"
	}
	p.config.Level = strings.ToUpper(p.config.Level)
	if p.config.Timeout == 0 {
		p.config.Timeout = 3
	}
	if p.config.Keepalive == 0 {
		p.config.Keepalive = 30
	}
	if p.config.BatchMaxSize == 0 {
		p.config.BatchMaxSize = 1000
	}
	if p.config.RetryDelay == 0 {
		p.config.RetryDelay = 1
	}
	if p.config.BufferDuration == 0 {
		p.config.BufferDuration = 60
	}
	if p.config.InactiveTimeout == 0 {
		p.config.InactiveTimeout = 3
	}
	if p.config.MaxPendingEntries == 0 {
		p.config.MaxPendingEntries = 10000
	}
	if p.config.TCP == nil && p.config.Host != "" {
		p.config.TCP = &TCPConfig{
			Host:          p.config.Host,
			Port:          p.config.Port,
			TLS:           p.config.TLS,
			TLSServerName: p.config.TLSServerName,
		}
	}
	if p.config.Skywalking != nil {
		if p.config.Skywalking.EndpointAddr == "" {
			p.config.Skywalking.EndpointAddr = "http://127.0.0.1:12900/v3/logs"
		}
		if p.config.Skywalking.ServiceName == "" {
			p.config.Skywalking.ServiceName = "APISIX"
		}
		if p.config.Skywalking.ServiceInstanceName == "" {
			p.config.Skywalking.ServiceInstanceName = "APISIX Service Instance"
		}
	}
	if p.config.Clickhouse != nil {
		if p.config.Clickhouse.EndpointAddr == "" {
			p.config.Clickhouse.EndpointAddr = "http://127.0.0.1:8123"
		}
		if p.config.Clickhouse.User == "" {
			p.config.Clickhouse.User = "default"
		}
	}
	if p.config.Kafka != nil {
		if p.config.Kafka.ProducerType == "" {
			p.config.Kafka.ProducerType = "async"
		}
		if p.config.Kafka.RequiredAcks == 0 {
			p.config.Kafka.RequiredAcks = 1
		}
		if p.config.Kafka.ClusterName == 0 {
			p.config.Kafka.ClusterName = 1
		}
		if p.config.Kafka.MetaRefreshInterval == 0 {
			p.config.Kafka.MetaRefreshInterval = 30
		}
	}
}

func (p *Plugin) filterLogs(lines []string) []string {
	threshold, ok := levelOrder[p.config.Level]
	if !ok {
		threshold = levelOrder["WARN"]
	}

	return lo.Filter(lines, func(line string, _ int) bool {
		level, ok := logLineLevel(line)
		return !ok || level <= threshold
	})
}

func logLineLevel(line string) (int, bool) {
	match := levelPattern.FindStringSubmatch(strings.ToLower(line))
	if len(match) != 2 {
		return 0, false
	}
	level, ok := levelOrder[strings.ToUpper(match[1])]
	return level, ok
}

func (p *Plugin) sendToTCP(ctx context.Context, lines []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := p.config.TCP
	if cfg == nil {
		return fmt.Errorf("missing tcp config")
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	timeout := time.Duration(p.config.Timeout) * time.Second
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	operationTimeout := time.Until(deadline)
	if operationTimeout <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}

	conn, err := p.dialTCPConnection(
		ctx,
		"tcp",
		addr,
		operationTimeout,
		&tls.Config{ServerName: cfg.TLSServerName},
		cfg.TLS,
	)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stopWatcher := watchConnectionCancellation(ctx, conn)
	defer stopWatcher()

	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}

	_, err = conn.Write([]byte(strings.Join(lines, "\n") + "\n"))
	return err
}

func (p *Plugin) dialTCPConnection(
	ctx context.Context,
	network, address string,
	timeout time.Duration,
	tlsConfig *tls.Config,
	useTLS bool,
) (net.Conn, error) {
	if p.dialTCP != nil {
		return p.dialTCP(ctx, network, address, timeout, tlsConfig, useTLS)
	}
	dialer := &net.Dialer{Timeout: timeout}
	if useTLS {
		handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		raw, err := dialer.DialContext(handshakeCtx, network, address)
		if err != nil {
			return nil, err
		}
		conn := tls.Client(raw, tlsConfig)
		if err := conn.HandshakeContext(handshakeCtx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		return conn, nil
	}
	return dialer.DialContext(ctx, network, address)
}

func watchConnectionCancellation(ctx context.Context, conn net.Conn) func() {
	if ctx == nil {
		return func() {}
	}
	done := make(chan struct{})
	var wait sync.WaitGroup
	wait.Go(func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	})
	return func() {
		close(done)
		wait.Wait()
	}
}

func (p *Plugin) sendToSkywalking(ctx context.Context, lines []string) error {
	entries := make([]skywalkingLogEntry, 0, len(lines))
	serviceInstanceName := p.skywalkingServiceInstanceName()
	for _, line := range lines {
		entries = append(entries, skywalkingLogEntry{
			Service:         p.config.Skywalking.ServiceName,
			ServiceInstance: serviceInstanceName,
			Endpoint:        "",
			Body: skywalkingLogBody{
				Text: skywalkingText{Text: line},
			},
		})
	}

	body, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.config.Skywalking.EndpointAddr,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return p.do(req)
}

func (p *Plugin) skywalkingServiceInstanceName() string {
	if p.config.Skywalking.ServiceInstanceName != "$hostname" {
		return p.config.Skywalking.ServiceInstanceName
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return p.config.Skywalking.ServiceInstanceName
	}
	return hostname
}

func (p *Plugin) sendToClickHouse(ctx context.Context, lines []string) error {
	return p.useClickHousePassword(func(password string) error {
		return p.sendToClickHouseWithPassword(ctx, lines, password)
	})
}

func (p *Plugin) useClickHousePassword(use func(string) error) error {
	if use == nil {
		return secret.ErrCredentialUnavailable
	}
	if p.clickhousePassword != nil {
		return p.clickhousePassword.Use(use)
	}
	if p.clickhousePasswordLegacy != nil {
		return use(*p.clickhousePasswordLegacy)
	}
	if p.metadataClickhousePassword != nil {
		return use(p.metadataClickhousePassword.plaintext)
	}
	return secret.ErrCredentialUnavailable
}

func (p *Plugin) sendToClickHouseWithPassword(ctx context.Context, lines []string, password string) error {
	entries := make([]string, 0, len(lines))
	for _, line := range lines {
		body, err := json.Marshal(map[string]string{"data": line})
		if err != nil {
			return err
		}
		entries = append(entries, string(body))
	}

	body := "INSERT INTO " + p.config.Clickhouse.LogTable + " FORMAT JSONEachRow " + strings.Join(entries, " ")
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		p.config.Clickhouse.EndpointAddr,
		strings.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ClickHouse-User", p.config.Clickhouse.User)
	req.Header.Set("X-ClickHouse-Key", password)
	req.Header.Set("X-ClickHouse-Database", p.config.Clickhouse.Database)
	return p.do(req)
}

func (p *Plugin) sendToKafka(ctx context.Context, lines []string) error {
	for _, line := range lines {
		body, err := json.Marshal(line)
		if err != nil {
			return err
		}
		if err := p.kafkaSender.Send(ctx, kafkaMessage{
			Topic: p.config.Kafka.KafkaTopic,
			Key:   []byte(p.config.Kafka.Key),
			Value: body,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Plugin) do(req *http.Request) error {
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("server returned status code %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) newKafkaWriter() (*kafka.Writer, error) {
	config := cloneKafkaConfig(p.config.Kafka)
	if config == nil {
		return nil, secret.ErrCredentialUnavailable
	}
	p.installLegacyKafkaPasswords(config)
	p.installMetadataKafkaPasswords(config)
	var writer *kafka.Writer
	err := p.withScopedKafkaPasswords(config, 0, func() error {
		var err error
		writer, err = p.newKafkaWriterForConfig(config)
		return err
	})
	return writer, err
}

func (p *Plugin) installLegacyKafkaPasswords(config *KafkaConfig) {
	for _, item := range p.kafkaPasswordsLegacy {
		if item.index >= 0 && item.index < len(config.Brokers) && config.Brokers[item.index].SASLConfig != nil {
			config.Brokers[item.index].SASLConfig.Password = item.value
		}
	}
}

func (p *Plugin) installMetadataKafkaPasswords(config *KafkaConfig) {
	for _, item := range p.metadataKafkaPasswords {
		if item.index >= 0 && item.index < len(config.Brokers) && config.Brokers[item.index].SASLConfig != nil {
			config.Brokers[item.index].SASLConfig.Password = item.value.plaintext
		}
	}
}

func (p *Plugin) withScopedKafkaPasswords(
	config *KafkaConfig,
	index int,
	use func() error,
) error {
	if index == len(p.kafkaPasswords) {
		if err := validateKafkaPasswordCoverage(
			config,
			p.kafkaPasswordsLegacy,
			p.kafkaPasswords,
			p.metadataKafkaPasswords,
		); err != nil {
			return err
		}
		return use()
	}
	item := p.kafkaPasswords[index]
	if item.index < 0 || item.index >= len(config.Brokers) || config.Brokers[item.index].SASLConfig == nil {
		return secret.ErrCredentialUnavailable
	}
	brokerIndex := item.index
	return item.value.Use(func(password string) error {
		config.Brokers[brokerIndex].SASLConfig.Password = password
		return p.withScopedKafkaPasswords(config, index+1, use)
	})
}

func validateKafkaPasswordCoverage(
	config *KafkaConfig,
	legacy []indexedLegacySecret,
	scoped []indexedSecret,
	metadata []indexedMetadataSecret,
) error {
	installed := make(map[int]struct{}, len(legacy)+len(scoped)+len(metadata))
	for _, item := range legacy {
		if item.index >= 0 && item.index < len(config.Brokers) && config.Brokers[item.index].SASLConfig != nil {
			installed[item.index] = struct{}{}
		}
	}
	for _, item := range scoped {
		if item.index >= 0 && item.index < len(config.Brokers) && config.Brokers[item.index].SASLConfig != nil {
			installed[item.index] = struct{}{}
		}
	}
	for _, item := range metadata {
		if item.index >= 0 && item.index < len(config.Brokers) && config.Brokers[item.index].SASLConfig != nil {
			installed[item.index] = struct{}{}
		}
	}
	for _, item := range scoped {
		if item.index < 0 || item.index >= len(config.Brokers) || config.Brokers[item.index].SASLConfig == nil {
			return secret.ErrCredentialUnavailable
		}
	}
	for index, broker := range config.Brokers {
		if broker.SASLConfig == nil {
			continue
		}
		if _, ok := installed[index]; !ok {
			return secret.ErrCredentialUnavailable
		}
	}
	return nil
}

func (p *Plugin) newKafkaWriterForConfig(config *KafkaConfig) (*kafka.Writer, error) {
	mechanism, err := saslMechanismFor(config)
	if err != nil {
		return nil, err
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(kafkaBrokerAddresses(config)...),
		RequiredAcks: kafka.RequiredAcks(config.RequiredAcks),
		Async:        config.ProducerType == "async",
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

func saslMechanismFor(config *KafkaConfig) (sasl.Mechanism, error) {
	if config == nil {
		return nil, nil
	}
	for _, broker := range config.Brokers {
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
		default:
			return nil, fmt.Errorf("unsupported Kafka SASL mechanism %q", broker.SASLConfig.Mechanism)
		}
	}

	return nil, nil
}

func kafkaBrokerAddresses(config *KafkaConfig) []string {
	if config == nil {
		return nil
	}
	addresses := make([]string, 0, len(config.Brokers))
	for _, broker := range config.Brokers {
		addresses = append(addresses, net.JoinHostPort(broker.Host, strconv.Itoa(broker.Port)))
	}
	sort.Strings(addresses)
	return addresses
}

func cloneKafkaConfig(config *KafkaConfig) *KafkaConfig {
	if config == nil {
		return nil
	}
	clone := *config
	clone.Brokers = append([]KafkaBroker(nil), config.Brokers...)
	for index, broker := range clone.Brokers {
		if broker.SASLConfig == nil {
			continue
		}
		saslConfig := *broker.SASLConfig
		clone.Brokers[index].SASLConfig = &saslConfig
	}
	return &clone
}

func (s *kafkaGoSender) Send(ctx context.Context, message kafkaMessage) error {
	return s.writer.WriteMessages(ctx, kafka.Message{
		Topic: message.Topic,
		Key:   message.Key,
		Value: message.Value,
	})
}

func (s *kafkaGoSender) Close() error {
	return s.writer.Close()
}

type skywalkingLogEntry struct {
	Service         string            `json:"service"`
	ServiceInstance string            `json:"serviceInstance"`
	Endpoint        string            `json:"endpoint"`
	Body            skywalkingLogBody `json:"body"`
}

type skywalkingLogBody struct {
	Text skywalkingText `json:"text"`
}

type skywalkingText struct {
	Text string `json:"text"`
}

func (p *Plugin) Send(log map[string]any) {
	body, err := json.Marshal(log)
	if err != nil {
		logger.Errorf("failed to marshal error log entry: %s", err)
		return
	}
	if p.BatchProcessor == nil {
		if err := p.SendLogs(context.Background(), []string{string(body)}); err != nil {
			logger.Errorf("failed to send error log entry: %s", err)
		}
		return
	}
	if !p.BatchProcessor.Push(map[string]any{"message": string(body)}) {
		logger.Errorf("failed to enqueue error log entry")
	}
}

func logLine(entry map[string]any) (string, error) {
	if line, ok := entry["message"].(string); ok {
		return line, nil
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("failed to marshal error log batch entry: %w", err)
	}
	return string(body), nil
}
