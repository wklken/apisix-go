package kafka_proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func validWebSocketUpgradeRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "http://example.test/kafka", nil)
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Upgrade", "WebSocket")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Version", "13")
	return request
}

func TestIsWebSocketUpgrade(t *testing.T) {
	tests := []struct {
		name  string
		build func() *http.Request
		want  bool
	}{
		{name: "valid mixed-case comma-separated", build: validWebSocketUpgradeRequest, want: true},
		{name: "nil request", build: func() *http.Request { return nil }},
		{name: "wrong method", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Method = http.MethodPost
			return request
		}},
		{name: "missing connection token", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Set("Connection", "keep-alive")
			return request
		}},
		{name: "missing upgrade header", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Del("Upgrade")
			return request
		}},
		{name: "missing key", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Del("Sec-WebSocket-Key")
			return request
		}},
		{name: "wrong version", build: func() *http.Request {
			request := validWebSocketUpgradeRequest()
			request.Header.Set("Sec-WebSocket-Version", "12")
			return request
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsWebSocketUpgrade(test.build()); got != test.want {
				t.Fatalf("IsWebSocketUpgrade() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestWebSocketProxyErrorWrapsAndFlagsHijack(t *testing.T) {
	cause := errors.New("kafka frame failed")
	wrapped := &websocketProxyError{hijacked: true, err: cause}

	if wrapped.Error() != cause.Error() {
		t.Fatalf("Error() = %q, want %q", wrapped.Error(), cause.Error())
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is() = false, want unwrapped cause")
	}
	if !WebSocketWasHijacked(wrapped) {
		t.Fatal("WebSocketWasHijacked() = false for a hijacked error")
	}
	if WebSocketWasHijacked(cause) {
		t.Fatal("WebSocketWasHijacked() = true for a plain error")
	}
	if WebSocketWasHijacked(&websocketProxyError{err: cause}) {
		t.Fatal("WebSocketWasHijacked() = true for a non-hijacked error")
	}
}

func TestWriteKafkaPayloadWritesCompleteFrames(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	payload := append(kafkaTestFrame([]byte("one")), kafkaTestFrame([]byte("two"))...)
	received := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(payload))
		_, _ = io.ReadFull(server, buf)
		received <- buf
	}()
	if err := writeKafkaPayload(t.Context(), client, payload, 1024, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := <-received; !bytes.Equal(got, payload) {
		t.Fatalf("received = %x, want %x", got, payload)
	}
}

func TestWriteKafkaPayloadRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "empty payload", payload: nil, want: "empty Kafka WebSocket message"},
		{name: "incomplete header", payload: []byte{0x00, 0x00}, want: "incomplete frame header"},
		{name: "oversized declared frame", payload: []byte{0x00, 0x00, 0x08, 0x00, 0x01, 0x02, 0x03, 0x04}, want: "exceeds max frame size"},
		{name: "incomplete body", payload: []byte{0x00, 0x00, 0x00, 0x08, 0x01, 0x02}, want: "incomplete frame payload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			err := writeKafkaPayload(t.Context(), client, test.payload, 1024, time.Second)
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte(test.want)) {
				t.Fatalf("writeKafkaPayload() error = %v, want fragment %q", err, test.want)
			}
		})
	}
}

func TestWriteKafkaPayloadHonorsExpiredContextDeadline(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := writeKafkaPayload(ctx, client, kafkaTestFrame([]byte("data")), 1024, time.Second); err == nil {
		t.Fatal("writeKafkaPayload() error = nil with an expired context deadline")
	}
}

func TestWriteKafkaPayloadFailsOnPeerClosure(t *testing.T) {
	client, server := net.Pipe()
	_ = server.Close()
	t.Cleanup(func() { _ = client.Close() })

	if err := writeKafkaPayload(t.Context(), client, kafkaTestFrame([]byte("data")), 1024, time.Second); err == nil {
		t.Fatal("writeKafkaPayload() error = nil after peer closed")
	}
}
