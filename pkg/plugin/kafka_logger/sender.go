package kafka_logger

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	kafka "github.com/segmentio/kafka-go"
	sasl "github.com/segmentio/kafka-go/sasl"
	plain "github.com/segmentio/kafka-go/sasl/plain"
	scram "github.com/segmentio/kafka-go/sasl/scram"
)

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

func (p *Plugin) newWriter(brokers []Broker) (*kafka.Writer, error) {
	mechanism, err := p.saslMechanism(brokers)
	if err != nil {
		return nil, err
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(p.brokerAddresses(brokers)...),
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

func (*Plugin) saslMechanism(brokers []Broker) (sasl.Mechanism, error) {
	for _, broker := range brokers {
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

func (p *Plugin) brokerAddresses(brokers []Broker) []string {
	addresses := make([]string, 0, len(brokers)+len(p.config.BrokerList))
	for _, broker := range brokers {
		addresses = append(addresses, net.JoinHostPort(broker.Host, strconv.Itoa(broker.Port)))
	}

	keys := make([]string, 0, len(p.config.BrokerList))
	for host := range p.config.BrokerList {
		keys = append(keys, host)
	}
	sort.Strings(keys)
	for _, host := range keys {
		addresses = append(addresses, net.JoinHostPort(host, strconv.Itoa(p.config.BrokerList[host])))
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

func (s *kafkaGoSender) Close() error {
	return s.writer.Close()
}
