package error_log_logger

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client         *http.Client
	kafkaSender    kafkaSender
	BatchProcessor *logger_batch.Processor
	stopOnce       sync.Once
}

const (
	priority = 1091
	name     = "error-log-logger"
)

const schema = `
{
  "type": "object"
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

	Name            string `json:"name,omitempty"`
	Level           string `json:"level,omitempty"`
	Timeout         int    `json:"timeout,omitempty"`
	Keepalive       int    `json:"keepalive,omitempty"`
	BatchMaxSize    int    `json:"batch_max_size,omitempty"`
	MaxRetryCount   int    `json:"max_retry_count,omitempty"`
	RetryDelay      int    `json:"retry_delay,omitempty"`
	BufferDuration  int    `json:"buffer_duration,omitempty"`
	InactiveTimeout int    `json:"inactive_timeout,omitempty"`
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
}

type kafkaGoSender struct {
	writer *kafka.Writer
}

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

	return nil
}

func (p *Plugin) PostInit() error {
	if err := p.resolveSecrets(); err != nil {
		return err
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
	p.BatchProcessor = logger_batch.New(logger_batch.Config{
		Name:            p.config.Name,
		BatchMaxSize:    p.config.BatchMaxSize,
		MaxRetryCount:   p.config.MaxRetryCount,
		RetryDelay:      time.Duration(p.config.RetryDelay) * time.Second,
		BufferDuration:  time.Duration(p.config.BufferDuration) * time.Second,
		InactiveTimeout: time.Duration(p.config.InactiveTimeout) * time.Second,
	}, p.SendBatch)

	return nil
}

func (p *Plugin) resolveSecrets() error {
	keyring, enabled := data_encryption.Keyring()
	resolver := data_encryption.NewResolver(enabled, keyring)
	if p.config.Clickhouse != nil {
		resolved, err := resolver.Resolve(p.config.Clickhouse.Password)
		if err != nil {
			return fmt.Errorf("error-log-logger clickhouse.password: %w", err)
		}
		p.config.Clickhouse.Password = resolved
	}
	if p.config.Kafka != nil {
		for i := range p.config.Kafka.Brokers {
			config := p.config.Kafka.Brokers[i].SASLConfig
			if config == nil {
				continue
			}
			resolved, err := resolver.Resolve(config.Password)
			if err != nil {
				return fmt.Errorf("error-log-logger kafka.brokers[%d].sasl_config.password: %w", i, err)
			}
			config.Password = resolved
		}
	}
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *Plugin) Stop() {
	p.stopOnce.Do(func() {
		if p.BatchProcessor != nil {
			p.BatchProcessor.Stop()
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
		return p.sendToTCP(filtered)
	}
}

func (p *Plugin) SendBatch(entries []map[string]any, _ int) (int, error) {
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line, err := logLine(entry)
		if err != nil {
			return 0, err
		}
		lines = append(lines, line)
	}
	return 0, p.SendLogs(context.Background(), lines)
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

	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		level, ok := logLineLevel(line)
		if !ok || level <= threshold {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func logLineLevel(line string) (int, bool) {
	match := levelPattern.FindStringSubmatch(strings.ToLower(line))
	if len(match) != 2 {
		return 0, false
	}
	level, ok := levelOrder[strings.ToUpper(match[1])]
	return level, ok
}

func (p *Plugin) sendToTCP(lines []string) error {
	cfg := p.config.TCP
	if cfg == nil {
		return fmt.Errorf("missing tcp config")
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	timeout := time.Duration(p.config.Timeout) * time.Second

	var conn net.Conn
	var err error
	if cfg.TLS {
		dialer := &net.Dialer{Timeout: timeout}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.TLSServerName})
	} else {
		conn, err = net.DialTimeout("tcp", addr, timeout)
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write([]byte(strings.Join(lines, "\n") + "\n"))
	return err
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
	req.Header.Set("X-ClickHouse-Key", p.config.Clickhouse.Password)
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
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("server returned status code %d", resp.StatusCode)
	}
	return nil
}

func (p *Plugin) newKafkaWriter() (*kafka.Writer, error) {
	mechanism, err := p.saslMechanism()
	if err != nil {
		return nil, err
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(p.kafkaBrokerAddresses()...),
		Topic:        p.config.Kafka.KafkaTopic,
		RequiredAcks: kafka.RequiredAcks(p.config.Kafka.RequiredAcks),
		Async:        p.config.Kafka.ProducerType == "async",
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
	for _, broker := range p.config.Kafka.Brokers {
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

func (p *Plugin) kafkaBrokerAddresses() []string {
	addresses := make([]string, 0, len(p.config.Kafka.Brokers))
	for _, broker := range p.config.Kafka.Brokers {
		addresses = append(addresses, net.JoinHostPort(broker.Host, fmt.Sprint(broker.Port)))
	}
	sort.Strings(addresses)
	return addresses
}

func (s *kafkaGoSender) Send(ctx context.Context, message kafkaMessage) error {
	return s.writer.WriteMessages(ctx, kafka.Message{
		Topic: message.Topic,
		Key:   message.Key,
		Value: message.Value,
	})
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
