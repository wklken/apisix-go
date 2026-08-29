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

type rocketMQWireSniffer struct {
	listener net.Listener
	records  chan [3]byte
	done     chan struct{}
}

func newRocketMQWireSniffer(t *testing.T, holdOpen bool) *rocketMQWireSniffer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sniffer := &rocketMQWireSniffer{
		listener: listener,
		records:  make(chan [3]byte, 8),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(sniffer.done)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if holdOpen {
				continue
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var record [3]byte
			_, readErr := io.ReadFull(conn, record[:])
			_ = conn.Close()
			if readErr == nil {
				sniffer.records <- record
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-sniffer.done
	})
	return sniffer
}

func (s *rocketMQWireSniffer) address() string {
	return s.listener.Addr().String()
}

func (s *rocketMQWireSniffer) nextRecord(t *testing.T) [3]byte {
	t.Helper()
	select {
	case record := <-s.records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("RocketMQ connection did not write a wire record")
		return [3]byte{}
	}
}

func (s *rocketMQWireSniffer) drainRecords() {
	for {
		select {
		case <-s.records:
		default:
			return
		}
	}
}

func isTLSRecord(record [3]byte) bool {
	return record[0] == 0x16 && record[1] == 0x03
}

func resetRocketMQDefaultTLSForTest(t *testing.T) {
	t.Helper()
	value, err := rocketmq.NewProducer(
		producer.WithNameServer([]string{"127.0.0.1:1"}),
		producer.WithTls(false),
	)
	if err != nil {
		t.Fatalf("reset RocketMQ default TLS: %v", err)
	}
	_ = value.Shutdown()
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

func TestRocketMQTLSIsIsolatedPerGeneration(t *testing.T) {
	t.Cleanup(func() { resetRocketMQDefaultTLSForTest(t) })
	plainWire := newRocketMQWireSniffer(t, false)
	tlsWire := newRocketMQWireSniffer(t, false)

	newSender := func(address string, useTLS bool) rocketmqSender {
		t.Helper()
		sender, err := (&Plugin{}).newSender(&Config{
			NameServerList: []string{address},
			Topic:          "apisix-logs",
			Timeout:        1,
			UseTLS:         useTLS,
		})
		if err != nil {
			t.Fatalf("newSender(use_tls=%t): %v", useTLS, err)
		}
		t.Cleanup(func() { shutdownRocketMQSender(sender) })
		return sender
	}
	send := func(sender rocketmqSender) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sender.Send(ctx, rocketmqMessage{Topic: "apisix-logs", Body: []byte("probe")})
	}

	plainSender := newSender(plainWire.address(), false)
	send(plainSender)
	if record := plainWire.nextRecord(t); isTLSRecord(record) {
		t.Fatal("plaintext generation unexpectedly used TLS before TLS generation was prepared")
	}
	time.Sleep(100 * time.Millisecond)
	plainWire.drainRecords()

	tlsSender := newSender(tlsWire.address(), true)
	send(tlsSender)
	if record := tlsWire.nextRecord(t); !isTLSRecord(record) {
		t.Fatalf("TLS generation wrote plaintext record %x", record)
	}
	time.Sleep(100 * time.Millisecond)
	tlsWire.drainRecords()
	plainWire.drainRecords()

	send(plainSender)
	if record := plainWire.nextRecord(t); isTLSRecord(record) {
		t.Fatal("preparing a TLS generation changed the active plaintext generation")
	}
}

func TestRocketMQTLSHandshakeHonorsSendContext(t *testing.T) {
	t.Cleanup(func() { resetRocketMQDefaultTLSForTest(t) })
	blackhole := newRocketMQWireSniffer(t, true)
	sender, err := (&Plugin{}).newSender(&Config{
		NameServerList: []string{blackhole.address()},
		Topic:          "apisix-logs",
		Timeout:        1,
		UseTLS:         true,
	})
	if err != nil {
		t.Fatalf("newSender() error = %v", err)
	}
	t.Cleanup(func() { shutdownRocketMQSender(sender) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = sender.Send(ctx, rocketmqMessage{Topic: "apisix-logs", Body: []byte("probe")})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("TLS handshake ignored send context: elapsed %s", elapsed)
	}
}
