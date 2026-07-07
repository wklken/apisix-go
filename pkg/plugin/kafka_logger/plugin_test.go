package kafka_logger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type captureSender struct {
	mu       sync.Mutex
	messages []kafkaMessage
}

func (s *captureSender) Send(ctx context.Context, message kafkaMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, message)
	return nil
}

func (s *captureSender) waitForMessage(t *testing.T) kafkaMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) > 0 {
			message := s.messages[0]
			s.mu.Unlock()
			return message
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for kafka message")
	return kafkaMessage{}
}

func newTestPlugin(t *testing.T, cfg Config, sender kafkaSender) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, sender: sender}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.sender = sender
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestSendEncodesLogAndPublishesToConfiguredTopic(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:    []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic: "apisix-logs",
		Key:        "route-a",
	}, sender)

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
	})

	message := sender.waitForMessage(t)
	if message.Topic != "apisix-logs" {
		t.Fatalf("topic = %q, want apisix-logs", message.Topic)
	}
	if string(message.Key) != "route-a" {
		t.Fatalf("key = %q, want route-a", string(message.Key))
	}

	var payload map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	if payload["route_id"] != "r1" {
		t.Fatalf("route_id = %v, want r1", payload["route_id"])
	}
	if payload["status"].(float64) != 200 {
		t.Fatalf("status = %v, want 200", payload["status"])
	}
}

func TestPostInitAcceptsDeprecatedBrokerListAndAppliesDefaults(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "apisix-logs",
	}, sender)

	got := p.brokerAddresses()
	if len(got) != 1 || got[0] != "127.0.0.1:9092" {
		t.Fatalf("broker addresses = %v, want [127.0.0.1:9092]", got)
	}
	if p.config.ProducerType != "async" {
		t.Fatalf("producer_type = %q, want async", p.config.ProducerType)
	}
	if p.config.RequiredAcks != 1 {
		t.Fatalf("required_acks = %d, want 1", p.config.RequiredAcks)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:    []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic: "apisix-logs",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "kafka-logger",
		},
	}, sender)

	req := httptest.NewRequest(http.MethodPatch, "http://example.com/orders/1?debug=true", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	if payload["method"] != http.MethodPatch {
		t.Fatalf("method = %v, want PATCH", payload["method"])
	}
	if payload["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %v, want request URI", payload["path"])
	}
	if payload["plugin"] != "kafka-logger" {
		t.Fatalf("plugin = %v, want kafka-logger", payload["plugin"])
	}
}
