package rocketmq_logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:   []string{"127.0.0.1:9876"},
		Topic:            "apisix-logs",
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	}, sender)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}

	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("payload request = %#v, want object", payload["request"])
	}
	if request["body"] != `{"order":1}` {
		t.Fatalf("payload request body = %#v, want original request body", request["body"])
	}

	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response = %#v, want object", payload["response"])
	}
	if response["body"] != `{"ok":true}` {
		t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
	}
}
