package error_log_logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	t.Cleanup(func() {
		if p.BatchProcessor != nil {
			p.BatchProcessor.Stop()
		}
	})

	return p
}

func TestSendLogsFiltersByLevelAndWritesTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
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
		defer conn.Close()
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

type fakeKafkaSender struct {
	messages []kafkaMessage
}

func (f *fakeKafkaSender) Send(_ context.Context, message kafkaMessage) error {
	f.messages = append(f.messages, message)
	return nil
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}
