package kafka_proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTransportRoundTripPreservesKafkaFrames(t *testing.T) {
	request := kafkaTestFrame([]byte("request-frame"))
	response := kafkaTestFrame([]byte("response-frame"))
	brokerErrors := make(chan error, 1)
	addr := startKafkaTestBroker(t, func(conn net.Conn) {
		defer conn.Close()
		got, err := io.ReadAll(io.LimitReader(conn, int64(len(request))))
		if err != nil {
			brokerErrors <- err
			return
		}
		if !bytes.Equal(got, request) {
			brokerErrors <- fmt.Errorf("request frame = %x, want %x", got, request)
			return
		}
		_, err = conn.Write(response)
		brokerErrors <- err
	})

	transport := NewTransport(TransportOptions{MaxFrameSize: 1024})
	got, err := transport.RoundTrip(context.Background(), "kafka://"+addr, request)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("response frame = %x, want %x", got, response)
	}
	if err := <-brokerErrors; err != nil {
		t.Fatalf("mock broker error: %v", err)
	}
}

func TestTransportRoundTripRejectsOversizedResponse(t *testing.T) {
	brokerErrors := make(chan error, 1)
	addr := startKafkaTestBroker(t, func(conn net.Conn) {
		defer conn.Close()
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			brokerErrors <- err
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(header[:]))); err != nil {
			brokerErrors <- err
			return
		}
		var response [4]byte
		binary.BigEndian.PutUint32(response[:], 128)
		_, err := conn.Write(response[:])
		brokerErrors <- err
	})

	transport := NewTransport(TransportOptions{MaxFrameSize: 32})
	_, err := transport.RoundTrip(context.Background(), "kafka://"+addr, kafkaTestFrame([]byte("request")))
	if err == nil || !strings.Contains(err.Error(), "exceeds max frame size") {
		t.Fatalf("RoundTrip() error = %v, want oversized response error", err)
	}
	if err := <-brokerErrors; err != nil {
		t.Fatalf("mock broker error: %v", err)
	}
}

func TestTransportRejectsUnsupportedTargetScheme(t *testing.T) {
	transport := NewTransport(TransportOptions{})
	_, err := transport.RoundTrip(context.Background(), "kafkas://127.0.0.1:9092", kafkaTestFrame([]byte("request")))
	if err == nil || !strings.Contains(err.Error(), "unsupported kafka target scheme") {
		t.Fatalf("RoundTrip() error = %v, want unsupported target scheme", err)
	}
}

func TestTransportRoundTripHonorsCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	transport := NewTransport(TransportOptions{ReadTimeout: time.Minute})
	go func() {
		_, roundTripErr := transport.RoundTrip(
			ctx,
			"kafka://"+listener.Addr().String(),
			kafkaTestFrame([]byte("request")),
		)
		result <- roundTripErr
	}()

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for mock broker connection")
	}
	cancel()

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("RoundTrip() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip() did not stop after cancellation")
	}
}

func startKafkaTestBroker(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			handler(conn)
		}
	}()
	return listener.Addr().String()
}

func kafkaTestFrame(payload []byte) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}
