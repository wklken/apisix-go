package rocketmq_logger

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/producer"
)

type startFailingRocketMQProducer struct {
	rocketmq.Producer
	shutdowns atomic.Int32
}

func (*startFailingRocketMQProducer) Start() error {
	return errors.New("start failed")
}

func (p *startFailingRocketMQProducer) Shutdown() error {
	p.shutdowns.Add(1)
	return nil
}

func TestNewSenderEnablesTLSForRocketMQConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientHello := make(chan struct{})
	var helloOnce sync.Once
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var recordHeader [3]byte
			_, readErr := io.ReadFull(conn, recordHeader[:])
			_ = conn.Close()
			if readErr == nil && recordHeader[0] == 0x16 && recordHeader[1] == 0x03 {
				helloOnce.Do(func() { close(clientHello) })
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-serverDone
	})

	p := &Plugin{}
	sender, err := p.newSender(&Config{
		NameServerList: []string{listener.Addr().String()},
		Topic:          "apisix-logs",
		Timeout:        1,
		UseTLS:         true,
	})
	if err != nil {
		t.Fatalf("newSender() error = %v", err)
	}
	t.Cleanup(func() { shutdownRocketMQSender(sender) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = sender.Send(ctx, rocketmqMessage{Topic: "apisix-logs", Body: []byte("probe")})

	select {
	case <-clientHello:
	case <-time.After(2 * time.Second):
		t.Fatal("RocketMQ connection did not send a TLS ClientHello")
	}
}

func TestNewSenderShutsDownProducerWhenStartFails(t *testing.T) {
	previous := newRocketMQProducer
	failing := &startFailingRocketMQProducer{}
	newRocketMQProducer = func(...producer.Option) (rocketmq.Producer, error) {
		return failing, nil
	}
	t.Cleanup(func() { newRocketMQProducer = previous })

	sender, err := (&Plugin{}).newSender(&Config{NameServerList: []string{"127.0.0.1:9876"}})
	if err == nil {
		t.Fatal("newSender() unexpectedly succeeded after producer Start failure")
	}
	if sender != nil {
		t.Fatal("newSender() returned a sender after producer Start failure")
	}
	if shutdowns := failing.shutdowns.Load(); shutdowns != 1 {
		t.Fatalf("producer Shutdown() calls = %d, want 1", shutdowns)
	}
}
