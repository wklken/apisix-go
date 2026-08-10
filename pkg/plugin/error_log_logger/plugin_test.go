package error_log_logger

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitRejectsInvalidEncryptedClickHousePassword(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{config: Config{Clickhouse: &ClickHouseConfig{Password: "not-a-ciphertext"}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want strict encrypted ClickHouse password rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedKafkaPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{newKey, oldKey})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	p := &Plugin{
		config: Config{Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "127.0.0.1",
				Port: 9092,
				SASLConfig: &SASLConfig{
					User:     "user",
					Password: encryptErrorLoggerTestValue(t, oldKey, "kafka-secret"),
				},
			}},
			KafkaTopic: "apisix-error-logs",
		}},
		kafkaSender: &fakeKafkaSender{},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(func() { p.Stop() })
	if got := p.config.Kafka.Brokers[0].SASLConfig.Password; got != "kafka-secret" {
		t.Fatalf("kafka password = %q, want resolved plaintext", got)
	}
}

func TestSendToTCPReturnsWithinWriteDeadline(t *testing.T) {
	conn := &deadlineWriteConn{}
	p := &Plugin{
		config: Config{
			TCP:     &TCPConfig{Host: "127.0.0.1", Port: 1},
			Timeout: 1,
		},
	}
	p.dialTCP = func(context.Context, string, string, time.Duration, *tls.Config, bool) (net.Conn, error) {
		return conn, nil
	}

	start := time.Now()
	err := p.sendToTCP(context.Background(), []string{"blocked write"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("sendToTCP() error = nil, want write deadline error")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("sendToTCP() took %s, want return within twice the configured 1s timeout", elapsed)
	}
}

func TestSendBatchHonorsParentContextForHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("canceled error-log request reached the backend")
	}))
	t.Cleanup(server.Close)

	p := &Plugin{config: Config{
		Skywalking: &SkywalkingConfig{EndpointAddr: server.URL},
		Timeout:    30,
	}, client: &http.Client{}}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.SendBatch(parent, []map[string]any{{"message": "line"}}, 1); err == nil {
		t.Fatal("SendBatch() error = nil, want canceled parent error")
	}
}

func TestSendToTCPHonorsParentDeadline(t *testing.T) {
	conn := &deadlineWriteConn{}
	p := &Plugin{
		config: Config{
			TCP:     &TCPConfig{Host: "127.0.0.1", Port: 1},
			Timeout: 30,
		},
	}
	p.dialTCP = func(context.Context, string, string, time.Duration, *tls.Config, bool) (net.Conn, error) {
		return conn, nil
	}

	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.sendToTCP(parent, []string{"blocked write"})
	if err == nil {
		t.Fatal("sendToTCP() error = nil, want parent deadline error")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("sendToTCP() took %s, want parent deadline to win", elapsed)
	}
}

func TestBatchProcessorDefaultsMaxPendingEntries(t *testing.T) {
	p := newTestPlugin(t, Config{
		BatchMaxSize:    50000,
		InactiveTimeout: 3600,
		BufferDuration:  3600,
	})

	dropped := 0
	for range 10002 {
		if !p.BatchProcessor.Push(map[string]any{"message": "line"}) {
			dropped++
		}
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want exactly 2 beyond the default 10000 pending cap", dropped)
	}
}

func TestMetadataSchemaAcceptsMaxPendingEntries(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{
		"tcp":                 map[string]any{"host": "127.0.0.1", "port": 1999},
		"max_pending_entries": 100,
	}, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected max_pending_entries: %v", err)
	}
}

func TestSendLogsFiltersByLevelAndWritesTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, Config{
		TCP:   &TCPConfig{Host: host, Port: mustAtoi(t, port)},
		Level: "WARN",
	})

	if err := p.SendLogs(context.Background(), []string{
		`2026/07/06 01:00:00 [info] skip`,
		`2026/07/06 01:00:01 [error] boom`,
		`2026/07/06 01:00:02 [warn] careful`,
	}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	got := <-received
	if strings.Contains(got, "skip") {
		t.Fatalf("tcp payload = %q, want info filtered out", got)
	}
	if !strings.Contains(got, "boom\n") || !strings.Contains(got, "careful\n") {
		t.Fatalf("tcp payload = %q, want error and warn lines", got)
	}
}

func TestSendLogsToSkyWalking(t *testing.T) {
	var entries []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/logs" {
			t.Fatalf("path = %q, want /v3/logs", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			t.Fatalf("decode skywalking entries: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking: &SkywalkingConfig{
			EndpointAddr:        server.URL + "/v3/logs",
			ServiceName:         "APISIX",
			ServiceInstanceName: "instance-a",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [warn] hello`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0]["service"] != "APISIX" || entries[0]["serviceInstance"] != "instance-a" {
		t.Fatalf("skywalking identity = %#v", entries[0])
	}
	body := entries[0]["body"].(map[string]any)
	text := body["text"].(map[string]any)
	if text["text"] != `2026/07/06 [warn] hello` {
		t.Fatalf("skywalking text = %v", text["text"])
	}
}

func TestSendLogsToSkyWalkingResolvesHostnameInstance(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}

	var entries []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			t.Fatalf("decode skywalking entries: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking: &SkywalkingConfig{
			EndpointAddr:        server.URL,
			ServiceInstanceName: "$hostname",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [warn] hello`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0]["serviceInstance"] != hostname {
		t.Fatalf("serviceInstance = %q, want hostname %q", entries[0]["serviceInstance"], hostname)
	}
}

func TestSendLogsToClickHouse(t *testing.T) {
	var body string
	var user string
	var key string
	var database string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body = string(buf)
		user = r.Header.Get("X-ClickHouse-User")
		key = r.Header.Get("X-ClickHouse-Key")
		database = r.Header.Get("X-ClickHouse-Database")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Clickhouse: &ClickHouseConfig{
			EndpointAddr: server.URL,
			User:         "default",
			Password:     "secret",
			Database:     "logs",
			LogTable:     "error_logs",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [error] boom`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if !strings.HasPrefix(body, "INSERT INTO error_logs FORMAT JSONEachRow ") {
		t.Fatalf("clickhouse body = %q", body)
	}
	if !strings.Contains(body, `{"data":"2026/07/06 [error] boom"}`) {
		t.Fatalf("clickhouse body = %q, want JSONEachRow data", body)
	}
	if user != "default" || key != "secret" || database != "logs" {
		t.Fatalf("clickhouse headers = %q/%q/%q", user, key, database)
	}
}

func TestSendLogsToKafka(t *testing.T) {
	sender := &fakeKafkaSender{}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
			Key:        "error",
		},
		Level: "ERROR",
	})
	p.kafkaSender = sender

	if err := p.SendLogs(context.Background(), []string{
		`2026/07/06 [warn] skip`,
		`2026/07/06 [error] boom`,
	}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(sender.messages) != 1 {
		t.Fatalf("kafka messages len = %d, want 1", len(sender.messages))
	}
	if sender.messages[0].Topic != "apisix-error-logs" || string(sender.messages[0].Key) != "error" {
		t.Fatalf("kafka message = %#v", sender.messages[0])
	}
	if string(sender.messages[0].Value) != `"2026/07/06 [error] boom"` {
		t.Fatalf("kafka value = %s", sender.messages[0].Value)
	}
}

func TestKafkaSASLMechanismDefaultsToPlain(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "127.0.0.1",
				Port: 9092,
				SASLConfig: &SASLConfig{
					User:     "user",
					Password: "pass",
				},
			}},
			KafkaTopic: "apisix-error-logs",
		},
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

func TestNewKafkaWriterUsesBrokerSASLConfig(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "127.0.0.1",
				Port: 9092,
				SASLConfig: &SASLConfig{
					Mechanism: "PLAIN",
					User:      "user",
					Password:  "pass",
				},
			}},
			KafkaTopic: "apisix-error-logs",
		},
	}}
	p.applyDefaults()

	writer, err := p.newKafkaWriter()
	if err != nil {
		t.Fatalf("newKafkaWriter() error = %v", err)
	}
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("writer does not have a SASL transport")
	}
	if got := transport.SASL.Name(); got != "PLAIN" {
		t.Fatalf("writer SASL mechanism = %q, want PLAIN", got)
	}
}

func TestNewKafkaWriterUsesMessageTopicOnly(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
	}}
	p.applyDefaults()

	writer, err := p.newKafkaWriter()
	if err != nil {
		t.Fatalf("newKafkaWriter() error = %v", err)
	}
	if writer.Topic != "" {
		t.Fatalf("writer.Topic = %q, want empty so kafkaMessage.Topic selects the destination", writer.Topic)
	}
}

func TestSendUsesBatchProcessor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, Config{
		TCP:             &TCPConfig{Host: host, Port: mustAtoi(t, port)},
		Level:           "INFO",
		BatchMaxSize:    2,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})

	p.Send(map[string]any{"message": "one"})
	select {
	case got := <-received:
		t.Fatalf("received payload before batch was full: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	p.Send(map[string]any{"message": "two"})

	select {
	case got := <-received:
		if !strings.Contains(got, `"message":"one"`) || !strings.Contains(got, `"message":"two"`) {
			t.Fatalf("tcp payload = %q, want both batched log entries", got)
		}
		if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 2 {
			t.Fatalf("tcp payload = %q, want two newline-delimited entries", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched tcp payload")
	}
}

func TestStartObservingForwardsApplicationLogsToCurrentOwner(t *testing.T) {
	firstListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen first sink: %v", err)
	}
	t.Cleanup(func() { _ = firstListener.Close() })
	firstHost, firstPortText, err := net.SplitHostPort(firstListener.Addr().String())
	if err != nil {
		t.Fatalf("split first sink: %v", err)
	}
	first := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: firstHost, Port: mustAtoi(t, firstPortText)},
		Level:        "WARN",
		BatchMaxSize: 1,
	})
	first.StartObserving()

	secondListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen second sink: %v", err)
	}
	t.Cleanup(func() { _ = secondListener.Close() })
	secondHost, secondPortText, err := net.SplitHostPort(secondListener.Addr().String())
	if err != nil {
		t.Fatalf("split second sink: %v", err)
	}
	second := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: secondHost, Port: mustAtoi(t, secondPortText)},
		Level:        "WARN",
		BatchMaxSize: 1,
	})
	second.StartObserving()

	first.Stop()
	received := make(chan string, 1)
	go func() {
		conn, acceptErr := secondListener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		body, _ := io.ReadAll(conn)
		received <- string(body)
	}()

	logger.Warn("standalone error-log observer marker")
	select {
	case payload := <-received:
		if !strings.Contains(payload, "[warn] standalone error-log observer marker") {
			t.Fatalf("payload = %q, want observed warning", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for current error-log observer")
	}
}

func TestStartObservingFiltersBeforeBatching(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen sink: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split sink: %v", err)
	}
	p := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: host, Port: mustAtoi(t, portText)},
		Level:        "WARN",
		BatchMaxSize: 2,
	})
	p.StartObserving()

	received := make(chan string, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		body, _ := io.ReadAll(conn)
		received <- string(body)
	}()

	logger.Info("filtered observer info")
	logger.Warn("first eligible observer warning")
	select {
	case payload := <-received:
		t.Fatalf("payload = %q before two eligible entries, low-level info counted toward batch size", payload)
	case <-time.After(100 * time.Millisecond):
	}

	logger.Error("second eligible observer error")
	select {
	case payload := <-received:
		if strings.Contains(payload, "filtered observer info") {
			t.Fatalf("payload = %q, want info filtered out", payload)
		}
		if !strings.Contains(payload, "first eligible observer warning") ||
			!strings.Contains(payload, "second eligible observer error") {
			t.Fatalf("payload = %q, want warning and error in one batch", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for two eligible log entries")
	}
}

func TestSendRetriesFailedBatch(t *testing.T) {
	var attempts atomic.Int32
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		close(done)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking:      &SkywalkingConfig{EndpointAddr: server.URL},
		Level:           "INFO",
		BatchMaxSize:    1,
		MaxRetryCount:   1,
		RetryDelay:      1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})

	p.Send(map[string]any{"message": "retry me"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for retry, attempts = %d", attempts.Load())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want first failure plus one retry", attempts.Load())
	}
}

func TestDefaultsMatchOfficialMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.Name != "error-log-logger" {
		t.Fatalf("name = %q, want error-log-logger", p.config.Name)
	}
	if p.config.Level != "WARN" || p.config.Timeout != 3 || p.config.Keepalive != 30 {
		t.Fatalf("defaults = level %q timeout %d keepalive %d", p.config.Level, p.config.Timeout, p.config.Keepalive)
	}
	if p.config.BatchMaxSize != 1000 || p.config.BufferDuration != 60 || p.config.InactiveTimeout != 3 {
		t.Fatalf("batch defaults = %d/%d/%d", p.config.BatchMaxSize, p.config.BufferDuration, p.config.InactiveTimeout)
	}
}

func TestMetadataSchemaRejectsTCPWithoutHost(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"tcp": map[string]any{"port": 1999},
	}, p.GetSchema())
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("Validate() error = %v, want missing tcp.host rejection", err)
	}
}

func TestMetadataSchemaRejectsMissingSink(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{"level": "WARN"}, p.GetSchema()); err == nil {
		t.Fatal("Validate() error = nil, want metadata without a sink rejected")
	}
}

type fakeKafkaSender struct {
	mu         sync.Mutex
	messages   []kafkaMessage
	closeCalls int
	closeErr   error
	blockSend  chan struct{}
}

func (f *fakeKafkaSender) Send(_ context.Context, message kafkaMessage) error {
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	if f.blockSend != nil {
		<-f.blockSend
	}
	return nil
}

func (f *fakeKafkaSender) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	return f.closeErr
}

func (f *fakeKafkaSender) messagesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeKafkaSender) closeCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

func TestStopUnregistersObserverAndClosesKafkaWriterOnce(t *testing.T) {
	captured := make(chan logger.Entry, 8)
	stopCapture := logger.ReplaceObserver("error-log-logger-close-test", func(entry logger.Entry) {
		captured <- entry
	})
	t.Cleanup(stopCapture)

	sender := &fakeKafkaSender{closeErr: fmt.Errorf("kafka close boom")}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
		Level:           "WARN",
		BatchMaxSize:    1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})
	p.kafkaSender = sender

	p.StartObserving()
	logger.Warn("delivered before stop")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "first delivery")

	p.Stop()
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls = %d, want 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case entry := <-captured:
			if strings.Contains(entry.Message, "kafka close boom") {
				goto closeErrorLogged
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("timed out waiting for the kafka close error log")
		}
	}
closeErrorLogged:

	// The observer was unregistered: nothing is delivered after Stop.
	logger.Warn("delivered after stop")
	time.Sleep(100 * time.Millisecond)
	if got := sender.messagesCount(); got != 1 {
		t.Fatalf("deliveries after Stop = %d, want the single pre-stop delivery", got)
	}

	// Stop is idempotent: the writer is closed exactly once.
	p.Stop()
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls after second Stop = %d, want 1", got)
	}
}

func TestStopWaitsForInflightKafkaSendBeforeClosing(t *testing.T) {
	sender := &fakeKafkaSender{blockSend: make(chan struct{})}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
		Level:           "WARN",
		BatchMaxSize:    1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})
	p.kafkaSender = sender

	p.StartObserving()
	logger.Warn("in-flight send")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "send in flight")

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()

	time.Sleep(100 * time.Millisecond)
	if got := sender.closeCallsCount(); got != 0 {
		t.Fatalf("kafka writer closed while a send was in flight, close calls = %d", got)
	}

	close(sender.blockSend)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Stop after the in-flight send completed")
	}
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls = %d, want 1 after join", got)
	}
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func encryptErrorLoggerTestValue(t *testing.T, key string, value string) string {
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

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}

type deadlineWriteConn struct {
	mu       sync.Mutex
	deadline time.Time
	closed   bool
}

func (c *deadlineWriteConn) Write([]byte) (int, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if deadline.IsZero() {
		time.Sleep(time.Hour)
		return 0, os.ErrDeadlineExceeded
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}

func (c *deadlineWriteConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *deadlineWriteConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *deadlineWriteConn) LocalAddr() net.Addr {
	return errorLoggerTestAddr("local")
}

func (c *deadlineWriteConn) RemoteAddr() net.Addr {
	return errorLoggerTestAddr("remote")
}

func (c *deadlineWriteConn) SetDeadline(time.Time) error {
	return nil
}

func (c *deadlineWriteConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *deadlineWriteConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	return nil
}

type errorLoggerTestAddr string

func (a errorLoggerTestAddr) Network() string {
	return "test"
}

func (a errorLoggerTestAddr) String() string {
	return string(a)
}
