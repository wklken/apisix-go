package rocketmq_logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func (s *captureSender) waitForMessages(t *testing.T, count int) []rocketmqMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) >= count {
			messages := append([]rocketmqMessage(nil), s.messages[:count]...)
			s.mu.Unlock()
			return messages
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d rocketmq messages", count)
	return nil
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
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestSendBatchEncodesEntriesAsSingleRocketMQMessage(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		Key:            "route-a",
		Tag:            "access",
		BatchMaxSize:   2,
	}, sender)

	firstFail, err := p.SendBatch([]map[string]any{{"route_id": "r1"}, {"route_id": "r2"}}, 2)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if firstFail != 0 {
		t.Fatalf("firstFail = %d, want 0", firstFail)
	}

	messages := sender.waitForMessages(t, 1)
	message := messages[0]
	if message.Key != "route-a" {
		t.Fatalf("key = %q, want route-a", message.Key)
	}
	if message.Tag != "access" {
		t.Fatalf("tag = %q, want access", message.Tag)
	}

	var payload []map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq batch payload: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("batch payload length = %d, want 2", len(payload))
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
		BatchMaxSize: 1,
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

func TestHandlerSendsOriginRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		MetaFormat:      "origin",
		NameServerList:  []string{"127.0.0.1:9876"},
		Topic:           "apisix-logs",
		IncludeReqBody:  true,
		MaxReqBodyBytes: 32,
		BatchMaxSize:    1,
	}, sender)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders?debug=true",
		bytes.NewBufferString(`{"order":1}`),
	)
	req.Header.Set("X-Tenant", "gold")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	payload := string(message.Body)
	if !strings.HasPrefix(payload, "POST /orders?debug=true HTTP/1.1\r\n") {
		t.Fatalf("origin payload = %q, want request line prefix", payload)
	}
	if !strings.Contains(payload, "X-Tenant: gold\r\n") {
		t.Fatalf("origin payload = %q, want request header", payload)
	}
	if !strings.HasSuffix(payload, "\r\n\r\n"+`{"order":1}`) {
		t.Fatalf("origin payload = %q, want request body after header block", payload)
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
		BatchMaxSize:     1,
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

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:      []string{"127.0.0.1:9876"},
		Topic:               "apisix-logs",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	}, sender)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}

	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("payload request = %#v, want object", payload["request"])
	}
	if request["body"] != `{"order":2}` {
		t.Fatalf("payload request body = %#v, want captured request body", request["body"])
	}

	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response = %#v, want object", payload["response"])
	}
	if response["body"] != `{"created":true}` {
		t.Fatalf("payload response body = %#v, want captured response body", response["body"])
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:      []string{"127.0.0.1:9876"},
		Topic:               "apisix-logs",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	}, sender)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	if _, ok := payload["request"]; ok {
		t.Fatalf("payload request = %#v, want no request body", payload["request"])
	}
	if _, ok := payload["response"]; ok {
		t.Fatalf("payload response = %#v, want no response body", payload["response"])
	}
}
