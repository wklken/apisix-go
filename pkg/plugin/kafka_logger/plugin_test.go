package kafka_logger

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/wklken/apisix-go/pkg/data_encryption"
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

func (s *captureSender) waitForMessages(t *testing.T, count int) []kafkaMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) >= count {
			messages := append([]kafkaMessage(nil), s.messages[:count]...)
			s.mu.Unlock()
			return messages
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d kafka messages", count)
	return nil
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
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestPostInitRejectsInvalidEncryptedSASLPassword(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{
		config: Config{
			Brokers: []Broker{{
				Host:       "127.0.0.1",
				Port:       9092,
				SASLConfig: &SASLConfig{User: "logger", Password: "not-a-ciphertext"},
			}},
			KafkaTopic: "apisix-logs",
		},
		sender: &captureSender{},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted SASL password rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedSASLPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	password := encryptKafkaLoggerTestValue(t, oldKey, "kafka-secret")
	p := &Plugin{
		config: Config{
			Brokers: []Broker{{
				Host:       "127.0.0.1",
				Port:       9092,
				SASLConfig: &SASLConfig{User: "logger", Password: password},
			}},
			KafkaTopic: "apisix-logs",
		},
		sender: &captureSender{},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.BatchProcessor.Stop() })
	if got := p.config.Brokers[0].SASLConfig.Password; got != "kafka-secret" {
		t.Fatalf("SASL password = %q, want resolved plaintext", got)
	}
}

func TestSendBatchEncodesEntriesAsSingleKafkaMessage(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:      []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:   "apisix-logs",
		Key:          "route-a",
		BatchMaxSize: 2,
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
	if string(message.Key) != "route-a" {
		t.Fatalf("key = %q, want route-a", string(message.Key))
	}

	var payload []map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka batch payload: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("batch payload length = %d, want 2", len(payload))
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

func TestHandlerSendsOriginRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		MetaFormat:      "origin",
		Brokers:         []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:      "apisix-logs",
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
	payload := string(message.Value)
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
		Brokers:          []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:       "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
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
		Brokers:             []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:          "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
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
		Brokers:             []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:          "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	if _, ok := payload["request"]; ok {
		t.Fatalf("payload request = %#v, want no request body", payload["request"])
	}
	if _, ok := payload["response"]; ok {
		t.Fatalf("payload response = %#v, want no response body", payload["response"])
	}
}

func TestSASLMechanismDefaultsToPlain(t *testing.T) {
	p := &Plugin{config: Config{
		Brokers: []Broker{{
			Host: "127.0.0.1",
			Port: 9092,
			SASLConfig: &SASLConfig{
				User:     "user",
				Password: "pass",
			},
		}},
		KafkaTopic: "apisix-logs",
	}}

	mechanism, err := p.saslMechanism()
	if err != nil {
		t.Fatalf("saslMechanism() error = %v", err)
	}
	if mechanism == nil {
		t.Fatal("saslMechanism() returned nil")
	}
	if got := mechanism.Name(); got != "PLAIN" {
		t.Fatalf("SASL mechanism = %q, want PLAIN", got)
	}
}

func TestNewWriterUsesBrokerSASLConfig(t *testing.T) {
	p := &Plugin{config: Config{
		Brokers: []Broker{{
			Host: "127.0.0.1",
			Port: 9092,
			SASLConfig: &SASLConfig{
				Mechanism: "SCRAM-SHA-512",
				User:      "user",
				Password:  "pass",
			},
		}},
		KafkaTopic: "apisix-logs",
	}}
	p.applyDefaults()

	writer, err := p.newWriter()
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("writer does not have a SASL transport")
	}
	if got := transport.SASL.Name(); got != "SCRAM-SHA-512" {
		t.Fatalf("writer SASL mechanism = %q, want SCRAM-SHA-512", got)
	}
}

func encryptKafkaLoggerTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
