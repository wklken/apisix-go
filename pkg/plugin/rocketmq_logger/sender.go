package rocketmq_logger

import (
	"context"
	"fmt"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
)

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

func (p *Plugin) newSender() (rocketmqSender, error) {
	options := []producer.Option{
		producer.WithNameServer(p.config.NameServerList),
		producer.WithSendMsgTimeout(time.Duration(p.config.Timeout) * time.Second),
		producer.WithInstanceName(fmt.Sprintf(
			"apisix-go-rocketmq-%d",
			producerInstanceSequence.Add(1),
		)),
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

	// SendSync is context-aware: its timeout/cancellation owns termination, so
	// no wrapper goroutine is needed and none can outlive the send.
	_, err := s.producer.SendSync(ctx, msg)
	return err
}
