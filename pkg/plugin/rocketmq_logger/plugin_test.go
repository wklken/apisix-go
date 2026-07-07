package rocketmq_logger

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
	messages []rocketmqMessage
}

func (s *captureSender) Send(ctx context.Context, message rocketmqMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, message)
	return nil
}

func (s *captureSender) waitForMessage(t *testing.T) rocketmqMessage {
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

	t.Fatal("timed out waiting for rocketmq message")
	return rocketmqMessage{}
}

func newTestPlugin(t *testing.T, cfg Config, sender rocketmqSender) *Plugin {
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
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		Key:            "route-a",
		Tag:            "access",
	}, sender)

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
	})

	message := sender.waitForMessage(t)
	if message.Topic != "apisix-logs" {
		t.Fatalf("topic = %q, want apisix-logs", message.Topic)
	}
	if message.Key != "route-a" {
		t.Fatalf("key = %q, want route-a", message.Key)
	}
	if message.Tag != "access" {
		t.Fatalf("tag = %q, want access", message.Tag)
	}

	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	if payload["route_id"] != "r1" {
		t.Fatalf("route_id = %v, want r1", payload["route_id"])
	}
	if payload["status"].(float64) != 200 {
		t.Fatalf("status = %v, want 200", payload["status"])
	}
}

func TestPostInitAppliesDefaults(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
	}, sender)

	if p.config.MetaFormat != "default" {
		t.Fatalf("meta_format = %q, want default", p.config.MetaFormat)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
	if p.config.MaxReqBodyBytes != 524288 {
		t.Fatalf("max_req_body_bytes = %d, want 524288", p.config.MaxReqBodyBytes)
	}
	if p.config.MaxRespBodyBytes != 524288 {
		t.Fatalf("max_resp_body_bytes = %d, want 524288", p.config.MaxRespBodyBytes)
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "rocketmq-logger",
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
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	if payload["method"] != http.MethodPatch {
		t.Fatalf("method = %v, want PATCH", payload["method"])
	}
	if payload["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %v, want request URI", payload["path"])
	}
	if payload["plugin"] != "rocketmq-logger" {
		t.Fatalf("plugin = %v, want rocketmq-logger", payload["plugin"])
	}
}
