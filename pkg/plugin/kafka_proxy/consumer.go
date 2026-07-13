package kafka_proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
)

const defaultKafkaFetchBytes = 16 << 20

// ConsumerOptions configures the Kafka consumer used by the official PubSub
// owner. SASL is deliberately limited to the APISIX 3.17 PLAIN contract.
type ConsumerOptions struct {
	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	MaxFetchBytes  int
	TLSConfig      *tls.Config
	SASLEnabled    bool
	SASLUsername   string
	SASLPassword   string
}

type KafkaConsumer interface {
	ListOffset(ctx context.Context, topic string, partition int32, timestamp int64) (int64, error)
	Fetch(ctx context.Context, topic string, partition int32, offset int64) ([]KafkaMessage, error)
}

type KafkaConsumerFactory func(context.Context, []string, ConsumerOptions) (KafkaConsumer, error)

type kafkaConsumer struct {
	brokers []string
	options ConsumerOptions
	dialer  *kafka.Dialer
}

func newKafkaConsumer(ctx context.Context, brokers []string, options ConsumerOptions) (KafkaConsumer, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("kafka upstream has no configured brokers")
	}
	if options.ConnectTimeout <= 0 {
		options.ConnectTimeout = defaultKafkaConnectTimeout
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = defaultKafkaReadTimeout
	}
	if options.MaxFetchBytes <= 0 {
		options.MaxFetchBytes = defaultKafkaFetchBytes
	}
	dialer := &kafka.Dialer{Timeout: options.ConnectTimeout}
	dialer.TLS = options.TLSConfig
	if options.SASLEnabled {
		dialer.SASLMechanism = plain.Mechanism{
			Username: options.SASLUsername,
			Password: options.SASLPassword,
		}
	}
	return &kafkaConsumer{brokers: append([]string(nil), brokers...), options: options, dialer: dialer}, nil
}

func (c *kafkaConsumer) ListOffset(ctx context.Context, topic string, partition int32, timestamp int64) (int64, error) {
	if topic == "" {
		return 0, fmt.Errorf("kafka topic is empty")
	}
	if partition < 0 {
		return 0, fmt.Errorf("kafka partition %d is negative", partition)
	}
	var offset int64
	err := c.withConn(ctx, topic, partition, func(conn *kafka.Conn) error {
		var err error
		switch timestamp {
		case -1:
			offset, err = conn.ReadLastOffset()
		case -2:
			offset, err = conn.ReadFirstOffset()
		case 0:
			offset, err = conn.ReadOffset(time.UnixMilli(0))
		default:
			if timestamp < 0 {
				return fmt.Errorf("kafka timestamp %d is unsupported", timestamp)
			}
			offset, err = conn.ReadOffset(time.UnixMilli(timestamp))
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("kafka list offset: %w", err)
	}
	return offset, nil
}

func (c *kafkaConsumer) Fetch(
	ctx context.Context,
	topic string,
	partition int32,
	offset int64,
) ([]KafkaMessage, error) {
	if topic == "" {
		return nil, fmt.Errorf("kafka topic is empty")
	}
	if partition < 0 {
		return nil, fmt.Errorf("kafka partition %d is negative", partition)
	}
	if offset < 0 {
		return nil, fmt.Errorf("kafka offset %d is negative", offset)
	}
	var messages []KafkaMessage
	err := c.withConn(ctx, topic, partition, func(conn *kafka.Conn) error {
		if _, err := conn.Seek(offset, kafka.SeekStart); err != nil {
			return err
		}
		batch := conn.ReadBatchWith(kafka.ReadBatchConfig{
			MinBytes: 1,
			MaxBytes: c.options.MaxFetchBytes,
			MaxWait:  c.options.ReadTimeout,
		})
		defer func() { _ = batch.Close() }()
		for {
			message, err := batch.ReadMessage()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			messages = append(messages, KafkaMessage{
				Offset:    message.Offset,
				Timestamp: message.Time.UnixMilli(),
				Key:       append([]byte(nil), message.Key...),
				Value:     append([]byte(nil), message.Value...),
			})
		}
	})
	if err != nil {
		return nil, fmt.Errorf("kafka fetch: %w", err)
	}
	return messages, nil
}

func (c *kafkaConsumer) withConn(
	ctx context.Context,
	topic string,
	partition int32,
	fn func(*kafka.Conn) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var lastErr error
	for _, broker := range c.brokers {
		address, err := kafkaTargetAddress(broker)
		if err != nil {
			lastErr = err
			continue
		}
		conn, err := c.dialer.DialLeader(ctx, "tcp", address, topic, int(partition))
		if err != nil {
			lastErr = err
			continue
		}
		stopCloseOnCancel := closeOnContextDone(ctx, conn)
		deadline := time.Now().Add(c.options.ReadTimeout)
		if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
			deadline = contextDeadline
		}
		_ = conn.SetDeadline(deadline)
		err = fn(conn)
		stopCloseOnCancel()
		_ = conn.Close()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		lastErr = err
	}
	if lastErr == nil {
		return fmt.Errorf("kafka upstream has no usable brokers")
	}
	return lastErr
}

var _ net.Conn = (*kafka.Conn)(nil)
