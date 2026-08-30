package tcp_logger

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type cancellationWatchConn struct {
	net.Conn
	closeCalls   atomic.Int32
	closeStarted chan struct{}
	releaseClose <-chan struct{}
}

func TestPostInitWarnsOnlyWhenTLSDisabled(t *testing.T) {
	for _, test := range []struct {
		name     string
		tls      bool
		wantWarn bool
	}{
		{name: "plain", wantWarn: true},
		{name: "tls", tls: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings []string
			stop := logger.ReplaceObserver(
				"tcp-logger-security-warning-"+test.name,
				func(entry logger.Entry) {
					if entry.Level == "WARN" &&
						strings.Contains(entry.Message, "tls disabled in tcp-logger") {
						warnings = append(warnings, entry.Message)
					}
				},
			)
			defer stop()

			newTestPlugin(t, Config{Host: "127.0.0.1", Port: 3000, TLS: test.tls})

			if test.wantWarn {
				if len(warnings) != 1 ||
					warnings[0] != "Keeping tls disabled in tcp-logger configuration is a security risk" {
					t.Fatalf("warnings = %#v, want exact disabled TLS warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %#v, want none with TLS enabled", warnings)
			}
		})
	}
}

func (c *cancellationWatchConn) Close() error {
	c.closeCalls.Add(1)
	select {
	case c.closeStarted <- struct{}{}:
	default:
	}
	if c.releaseClose != nil {
		<-c.releaseClose
	}
	return nil
}

func TestWatchConnectionCancellation(t *testing.T) {
	for _, scenario := range []string{
		"cancellation closes connection",
		"normal completion preserves reused connection",
		"cleanup joins close already running",
		"nil context is no-op",
	} {
		t.Run(scenario, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario != "nil context is no-op" {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			var releaseClose chan struct{}
			if scenario == "cleanup joins close already running" {
				releaseClose = make(chan struct{})
			}
			conn := &cancellationWatchConn{
				closeStarted: make(chan struct{}, 1),
				releaseClose: releaseClose,
			}
			cleanup := watchConnectionCancellation(ctx, conn)

			switch scenario {
			case "cancellation closes connection":
				cancel()
				cleanup()
			case "normal completion preserves reused connection":
				cleanup()
				cancel()
			case "cleanup joins close already running":
				cancel()
				select {
				case <-conn.closeStarted:
				case <-time.After(time.Second):
					t.Fatal("cancellation callback did not enter Close")
				}
				cleanupDone := make(chan struct{})
				go func() {
					cleanup()
					close(cleanupDone)
				}()
				select {
				case <-cleanupDone:
					t.Fatal("cleanup returned while Close was blocked")
				case <-time.After(20 * time.Millisecond):
				}
				close(releaseClose)
				select {
				case <-cleanupDone:
				case <-time.After(time.Second):
					t.Fatal("cleanup did not return after Close completed")
				}
			case "nil context is no-op":
				cleanup()
			}

			wantCloseCalls := int32(0)
			if scenario == "cancellation closes connection" ||
				scenario == "cleanup joins close already running" {
				wantCloseCalls = 1
			}
			if got := conn.closeCalls.Load(); got != wantCloseCalls {
				t.Fatalf("Close calls = %d, want %d", got, wantCloseCalls)
			}
		})
	}
}

func TestRunLogPhasePreservesDefaultAndCustomAccessFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{config: Config{}, BaseLoggerPlugin: base.BaseLoggerPlugin{RouteID: "route-1"}}
	p.logFormat = map[string]any{
		"host": "$host", "remote": "$remote_addr", "started": "$time_iso8601", "status": "$status",
	}
	p.SetSnapshotLogFormat(p.logFormat, nil)
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	started := time.Unix(100, 0)
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet, URI: "/orders", Host: "gateway.test:9443",
			RemoteAddr: "192.0.2.4:3210", Scheme: "https", Proto: "HTTP/2.0",
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusAccepted, Bytes: 4},
		Started: started, Finished: started.Add(time.Second),
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case fields := <-delivered:
		if fields["host"] != "gateway.test" || fields["remote"] != "192.0.2.4" ||
			fields["started"] != started.Format(time.RFC3339) ||
			fields["status"] != http.StatusAccepted {
			t.Fatalf("custom fields = %#v", fields)
		}
	case <-time.After(time.Second):
		t.Fatal("detached TCP entry was not delivered")
	}
	p.logFormat = nil
	fields := base.BuildAccessLogFromSnapshot(snapshot, "route-1")
	if fields["route_id"] != "route-1" ||
		fields["response"].(map[string]any)["status"] != http.StatusAccepted {
		t.Fatalf("default fields = %#v", fields)
	}
}

func TestSendBodyUnblocksOnParentCancellation(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
	})

	p := &Plugin{config: Config{Timeout: 30}, conn: client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.sendBody(ctx, []byte("blocked")) }()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("sendBody() error = nil, want cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("sendBody() remained blocked after parent cancellation")
	}

	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		t.Fatal("sendBody() retained a canceled connection")
	}
}

func TestEncodeBatchPreservesTCPMarshalErrorContext(t *testing.T) {
	badEntry := map[string]any{"bad": make(chan int)}
	tests := []struct {
		name         string
		entries      []map[string]any
		batchMaxSize int
		want         string
	}{
		{
			name:         "single",
			entries:      []map[string]any{badEntry},
			batchMaxSize: 1,
			want:         "failed to marshal tcp log entry",
		},
		{
			name:         "batch",
			entries:      []map[string]any{badEntry},
			batchMaxSize: 2,
			want:         "failed to marshal tcp log entries",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := encodeBatch(tt.entries, tt.batchMaxSize)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("encodeBatch() error = %v, want %q context", err, tt.want)
			}
		})
	}
}

func TestSendWritesTCPMessage(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port), Timeout: 1000})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp log message")
	}
}

func TestSendWritesTLSMessageWithServerName(t *testing.T) {
	addr, received, serverNames := startTLSServer(t)
	host, port := splitAddr(t, addr)
	serverName := "logs.example.test"

	var config Config
	if err := util.Parse(map[string]any{
		"host":        host,
		"port":        mustAtoi(t, port),
		"tls":         true,
		"tls_options": serverName,
		"ssl_verify":  false,
		"timeout":     1000,
	}, &config); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, config)
	p.Send(map[string]any{"path": "/secure"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/secure"`) {
			t.Fatalf("message = %q, want JSON log entry", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls log message")
	}

	select {
	case got := <-serverNames:
		if got != serverName {
			t.Fatalf("SNI = %q, want %q", got, serverName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tls server name")
	}
}

func TestSendRejectsUntrustedTLSMessageByDefault(t *testing.T) {
	addr, _, _ := startTLSServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:    host,
		Port:    mustAtoi(t, port),
		TLS:     true,
		Timeout: 1000,
	})
	if err := p.sendBody(context.Background(), []byte("secure")); err == nil {
		t.Fatal("sendBody() error = nil, want untrusted TLS peer rejection")
	}
}

func TestTLSUsesHostAsServerNameWhenVerificationEnabled(t *testing.T) {
	addr, _, serverNames := startTLSServer(t)
	_, port := splitAddr(t, addr)
	blankServerName := ""

	p := newTestPlugin(t, Config{
		Host:       "localhost",
		Port:       mustAtoi(t, port),
		TLS:        true,
		TLSOptions: &blankServerName,
		Timeout:    1000,
	})
	if err := p.sendBody(context.Background(), []byte("secure")); err == nil {
		t.Fatal("sendBody() error = nil, want untrusted TLS peer rejection")
	}

	select {
	case got := <-serverNames:
		if got != "localhost" {
			t.Fatalf("SNI = %q, want configured host localhost", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host-derived TLS server name")
	}
}

func TestPostInitAppliesBatchDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 9})

	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatalf("SSLVerify = %v, want true", p.config.SSLVerify)
	}
}

func TestPostInitPreservesExplicitZeroRetryDelay(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"host":        "127.0.0.1",
		"port":        9,
		"retry_delay": 0,
	}, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay = %d, want explicit zero preserved", p.config.RetryDelay)
	}
}

func TestHandlerBatchesTCPLogs(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Timeout:         1000,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://example.com/one", nil),
	)
	handler.ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "http://example.com/two", nil),
	)

	select {
	case message := <-received:
		var payload []map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP batch payload: %v, message=%q", err, message)
		}
		if len(payload) != 2 {
			t.Fatalf("batch length = %d, want 2", len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tcp batch message")
	}
}

func TestHandlerDefaultLogMatchesAPISIXFullLogShape(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
	})
	p.SetRouteContext("route-default", "127.0.0.1:9080")

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/orders?ID=1", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.10:54321"
	req.Header.Set("X-Request", "request-value")
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "198.51.100.20")
		apisixctx.RegisterApisixVar(r, "$balancer_port", "1980")
		apisixctx.RegisterApisixVar(r, "$service_id", "service-default")
		apisixctx.RegisterApisixVar(r, "$consumer_name", "alice")
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(7))
		w.Header().Set("X-Upstream", "response-value")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	assertNestedField(t, payload, "request", "url", "http://gateway.example:9080/orders?ID=1")
	assertNestedField(t, payload, "request", "method", http.MethodGet)
	assertNestedField(t, payload, "request", "uri", "/orders?ID=1")
	assertNestedNonnegativeNumber(t, payload, "request", "size")
	assertNestedField(t, payload, "response", "status", float64(http.StatusCreated))
	assertNestedNonnegativeNumber(t, payload, "response", "size")
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname() error = %v", err)
	}
	assertNestedField(t, payload, "server", "hostname", hostname)
	assertNestedField(t, payload, "server", "version", "apisix-go")
	if payload["service_id"] != "service-default" {
		t.Fatalf("service_id = %#v, want service-default", payload["service_id"])
	}
	if payload["route_id"] != "route-default" {
		t.Fatalf("route_id = %#v, want route-default", payload["route_id"])
	}
	assertNestedField(t, payload, "consumer", "username", "alice")
	if payload["client_ip"] != "192.0.2.10" {
		t.Fatalf("client_ip = %#v, want port-free address", payload["client_ip"])
	}
	if payload["upstream"] != "198.51.100.20:1980" {
		t.Fatalf("upstream = %#v, want selected upstream", payload["upstream"])
	}
	if payload["upstream_latency"] != float64(7) {
		t.Fatalf("upstream_latency = %#v, want 7", payload["upstream_latency"])
	}
	for _, field := range []string{"start_time", "latency", "apisix_latency"} {
		if _, ok := payload[field].(float64); !ok {
			t.Fatalf("%s = %#v, want numeric milliseconds", field, payload[field])
		}
	}
	requestLog := payload["request"].(map[string]any)
	requestHeaders := requestLog["headers"].(map[string]any)
	if requestHeaders["host"] != "gateway.example" {
		t.Errorf("request.headers.host = %#v, want original request host", requestHeaders["host"])
	}
	if requestHeaders["x-request"] != "request-value" {
		t.Fatalf(
			"request.headers.x-request = %#v, want scalar request-value",
			requestHeaders["x-request"],
		)
	}
	queryString := requestLog["querystring"].(map[string]any)
	if queryString["ID"] != "1" {
		t.Errorf("request.querystring.ID = %#v, want case-preserved scalar 1", queryString["ID"])
	}
	if _, ok := queryString["id"]; ok {
		t.Errorf("request.querystring.id = %#v, want uppercase key preserved", queryString["id"])
	}
	responseLog := payload["response"].(map[string]any)
	responseHeaders := responseLog["headers"].(map[string]any)
	if responseHeaders["x-upstream"] != "response-value" {
		t.Fatalf(
			"response.headers.x-upstream = %#v, want scalar response-value",
			responseHeaders["x-upstream"],
		)
	}
	for _, field := range []string{"route", "client", "timing"} {
		if _, ok := payload[field]; ok {
			t.Fatalf("%s = %#v, want APISIX flat full-log contract", field, payload[field])
		}
	}
}

func TestHandlerResolvesCustomFormatAfterDownstream(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"status":     "$status",
			"consumer":   "$consumer_name",
			"route_id":   "configured-route",
			"service_id": "configured-service",
		},
	})
	p.SetRouteContext("resolved-route", "127.0.0.1:9080")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$consumer_name", "downstream-consumer")
		apisixctx.RegisterApisixVar(r, "$service_id", "resolved-service")
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	if payload["status"] != float64(http.StatusCreated) {
		t.Fatalf("status = %#v, want downstream status", payload["status"])
	}
	if payload["consumer"] != "downstream-consumer" {
		t.Fatalf("consumer = %#v, want downstream-populated value", payload["consumer"])
	}
	if payload["route_id"] != "resolved-route" {
		t.Fatalf("route_id = %#v, want appended route context", payload["route_id"])
	}
	if payload["service_id"] != "resolved-service" {
		t.Fatalf("service_id = %#v, want appended service context", payload["service_id"])
	}
}

func TestHandlerResolvesNestedCustomFormat(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	var cfg Config
	if err := util.Parse(map[string]any{
		"host":           host,
		"port":           mustAtoi(t, port),
		"timeout":        1000,
		"batch_max_size": 1,
		"log_format": map[string]any{
			"request": map[string]any{
				"host": "$host",
				"client": map[string]any{
					"ip": "$remote_addr",
				},
			},
			"response": map[string]any{
				"status": "$status",
			},
		},
	}, &cfg); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	p := newTestPlugin(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.12:54321"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	assertNestedField(t, payload, "request", "host", "gateway.example")
	request := payload["request"].(map[string]any)
	assertNestedField(t, request, "client", "ip", "192.0.2.12")
	assertNestedField(t, payload, "response", "status", float64(http.StatusCreated))
}

func TestHandlerTruncatesCustomFormatAfterDepthFive(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"within": map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"host": "$host",
						},
					},
				},
			},
			"beyond": map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"d": map[string]any{
								"deep": "$host",
							},
						},
					},
				},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req.Host = "gateway.example"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	within := payload["within"].(map[string]any)["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)
	if within["host"] != "gateway.example" {
		t.Fatalf("depth-four host = %#v, want resolved boundary value", within["host"])
	}
	beyond := payload["beyond"].(map[string]any)["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"].(map[string]any)
	if len(beyond) != 0 {
		t.Fatalf("depth-five object = %#v, want truncated empty map", beyond)
	}
}

func TestHandlerCustomFormatOmitsAbsentServiceID(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port := splitAddr(t, addr)

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      1000,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"case":       "no-service",
			"service_id": "stale-service",
		},
	})
	p.SetRouteContext("route-without-service", "127.0.0.1:9080")
	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/hello", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), req)

	payload := waitForTCPPayload(t, received)
	if _, ok := payload["service_id"]; ok {
		t.Fatalf(
			"service_id = %#v, want field omitted without service context",
			payload["service_id"],
		)
	}
}

func TestSendBodyConnectionErrorIncludesDestination(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 1, Timeout: 20})

	err := p.sendBody(context.Background(), []byte("log"))
	if err == nil {
		t.Fatal("sendBody() error = nil, want invalid destination error")
	}
	if !strings.Contains(err.Error(), "failed to connect to TCP server: host[127.0.0.1] port[1]") {
		t.Fatalf("sendBody() error = %v, want source-compatible host and port diagnostic", err)
	}
}

func TestHandlerBodyCaptureMatrix(t *testing.T) {
	tests := []struct {
		name, requestBody, responseBody, header string
		requestExpr, responseExpr               []any
		wantBodies                              bool
	}{
		{name: "unconditional", requestBody: `{"order":1}`, responseBody: `{"ok":true}`, wantBodies: true},
		{
			name:         "expressions match",
			requestBody:  `{"order":2}`,
			responseBody: `{"created":true}`,
			header:       "yes",
			requestExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
			responseExpr: []any{[]any{"status", "==", "201"}},
			wantBodies:   true,
		},
		{
			name:         "expressions miss",
			requestBody:  `{"order":3}`,
			responseBody: `{"created":false}`,
			header:       "no",
			requestExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
			responseExpr: []any{[]any{"status", "==", "500"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr, received := startTCPServer(t)
			host, port := splitAddr(t, addr)
			p := newTestPlugin(t, Config{
				Host: host, Port: mustAtoi(t, port), Timeout: 1000, BatchMaxSize: 1,
				IncludeReqBody: true, IncludeReqBodyExpr: test.requestExpr,
				IncludeRespBody: true, IncludeRespBodyExpr: test.responseExpr,
				MaxReqBodyBytes: 32, MaxRespBodyBytes: 32,
			})

			req := httptest.NewRequest(
				http.MethodPost,
				"http://example.com/orders",
				strings.NewReader(test.requestBody),
			)
			if test.header != "" {
				req.Header.Set("X-Log-Body", test.header)
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("upstream read body: %v", err)
				}
				if string(body) != test.requestBody {
					t.Fatalf("upstream request body = %q, want %q", body, test.requestBody)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(test.responseBody))
			})).ServeHTTP(rr, req)
			if rr.Body.String() != test.responseBody {
				t.Fatalf("response body = %q, want %q", rr.Body.String(), test.responseBody)
			}

			select {
			case message := <-received:
				var payload map[string]any
				if err := json.Unmarshal([]byte(message), &payload); err != nil {
					t.Fatalf("unmarshal TCP log payload: %v", err)
				}
				request := payload["request"].(map[string]any)
				response := payload["response"].(map[string]any)
				if test.wantBodies {
					if request["body"] != test.requestBody || response["body"] != test.responseBody {
						t.Fatalf(
							"logged bodies = (%#v, %#v), want (%q, %q)",
							request["body"],
							response["body"],
							test.requestBody,
							test.responseBody,
						)
					}
				} else if _, requestOK := request["body"]; requestOK {
					t.Fatalf("request.body = %#v, want omitted", request["body"])
				} else if _, responseOK := response["body"]; responseOK {
					t.Fatalf("response.body = %#v, want omitted", response["body"])
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for tcp log message")
			}
		})
	}
}

func TestSchemaAcceptsOfficialBodySizeFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                "127.0.0.1",
		"port":                9000,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body size fields: %v", err)
	}
}

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                "127.0.0.1",
		"port":                9000,
		"batch_max_size":      10,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     2,
		"inactive_timeout":    1,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official batch fields: %v", err)
	}
}

func TestSchemaAcceptsSSLVerify(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{
		"host":       "127.0.0.1",
		"port":       9000,
		"ssl_verify": false,
	}, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected ssl_verify: %v", err)
	}
}

func TestMetadataSchemaRejectsStringLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"log_format": "'$host' '$time_iso8601'",
	}, p.GetMetadataSchema())
	if err == nil {
		t.Fatal("metadata schema accepted string log_format, want object validation error")
	}
	if !strings.Contains(err.Error(), "log_format") || !strings.Contains(err.Error(), "object") {
		t.Fatalf("metadata schema error = %v, want log_format object validation error", err)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(
		base.Dependencies{
			Tasks:    newLoggerTestTaskOwner(t),
			Metadata: mustMetadataView(t, metadata),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	config := Config{Host: "127.0.0.1", Port: 9000}
	first := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format": map[string]any{
			"nested": map[string]any{"generation": "n"},
		},
		"max_pending_entries": 11,
	})
	second := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format": map[string]any{
			"nested": map[string]any{"generation": "n-plus-one"},
		},
		"max_pending_entries": 12,
	})
	firstNested, firstOK := first.logFormat["nested"].(map[string]any)
	secondNested, secondOK := second.logFormat["nested"].(map[string]any)
	if !firstOK || !secondOK {
		t.Fatalf("generation metadata = %#v/%#v", first.logFormat, second.logFormat)
	}
	if firstNested["generation"] != "n" || first.config.MaxPendingEntries != 11 {
		t.Fatalf("generation N metadata = %#v/%d", firstNested, first.config.MaxPendingEntries)
	}
	if secondNested["generation"] != "n-plus-one" || second.config.MaxPendingEntries != 12 {
		t.Fatalf("generation N+1 metadata = %#v/%d", secondNested, second.config.MaxPendingEntries)
	}
}

func TestMetadataDecodeFailsBeforeTCPProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{Host: "127.0.0.1", Port: 9000}}
	p.SetDependencies(
		base.Dependencies{
			Tasks: newLoggerTestTaskOwner(t),
			Metadata: mustMetadataView(t, map[string]any{
				"max_pending_entries": "invalid",
			}),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.BatchProcessor != nil || p.conn != nil {
		t.Fatalf(
			"decode failure acquired TCP resources: processor=%v conn=%v",
			p.BatchProcessor,
			p.conn,
		)
	}
}

// countingTCPListener accepts connections indefinitely, records every byte
// received per connection, and can break the newest connection with a RST so
// the client observes a broken pipe deterministically.
type countingTCPListener struct {
	ln       net.Listener
	mu       sync.Mutex
	accepted int
	payloads [][]byte
	conns    []net.Conn
}

func newCountingTCPListener(t *testing.T) *countingTCPListener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	server := &countingTCPListener{ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		server.mu.Lock()
		for _, conn := range server.conns {
			_ = conn.Close()
		}
		server.mu.Unlock()
	})
	go server.acceptLoop()
	return server
}

func (s *countingTCPListener) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		index := len(s.conns)
		s.conns = append(s.conns, conn)
		s.accepted++
		s.mu.Unlock()
		go s.readLoop(index, conn)
	}
}

func (s *countingTCPListener) readLoop(index int, conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := conn.Read(buf)
		if n > 0 {
			s.mu.Lock()
			for len(s.payloads) <= index {
				s.payloads = append(s.payloads, nil)
			}
			payload := append([]byte(nil), s.payloads[index]...)
			s.payloads[index] = append(payload, buf[:n]...)
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *countingTCPListener) addr() string {
	return s.ln.Addr().String()
}

func (s *countingTCPListener) acceptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func (s *countingTCPListener) payload(index int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < 0 || index >= len(s.payloads) {
		return ""
	}
	return string(s.payloads[index])
}

func (s *countingTCPListener) breakNewestConnection() {
	s.mu.Lock()
	conn := s.conns[len(s.conns)-1]
	s.mu.Unlock()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = conn.Close()
}

func TestSendBatchReusesConnectionAndRedialsOnce(t *testing.T) {
	server := newCountingTCPListener(t)
	host, port := splitAddr(t, server.addr())

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port), Timeout: 1000})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch #1 error = %v", err)
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r2"}}, 1); err != nil {
		t.Fatalf("SendBatch #2 error = %v", err)
	}
	waitForTCP(t, func() bool {
		return server.acceptCount() == 1 && strings.Contains(server.payload(0), `"route_id":"r2"`)
	})
	if got := server.acceptCount(); got != 1 {
		t.Fatalf("connections after two batches = %d, want 1 reused connection", got)
	}

	server.breakNewestConnection()
	// Read once so the client socket has processed the RST before the next
	// send; the read returns the reset error deterministically.
	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(make([]byte, 16))
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r3"}}, 1); err == nil {
		t.Fatal("SendBatch #3 error = nil on a broken connection")
	}
	if got := server.acceptCount(); got != 1 {
		t.Fatalf("connections after failed batch = %d, want no redial until the next send", got)
	}

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r4"}}, 1); err != nil {
		t.Fatalf("SendBatch #4 error = %v, want redial delivery", err)
	}
	waitForTCP(t, func() bool {
		return server.acceptCount() == 2 && strings.Contains(server.payload(1), `"route_id":"r4"`)
	})
	if got := server.acceptCount(); got != 2 {
		t.Fatalf("connections after redial = %d, want exactly 2", got)
	}

	if payload := server.payload(0); !strings.Contains(payload, `"route_id":"r1"`) ||
		!strings.Contains(payload, `"route_id":"r2"`) {
		t.Fatalf("first connection payload = %q, want both reused batches", payload)
	}
	if payload := server.payload(1); !strings.Contains(payload, `"route_id":"r4"`) {
		t.Fatalf("redial connection payload = %q, want the post-redial batch", payload)
	}
}

func waitForTCP(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for tcp transport condition")
}

// shortWriteConn delivers at most one byte per Write call so a transport that
// assumes a single full write truncates the payload, and records deadline
// calls for observability.
type shortWriteConn struct {
	net.Conn
	writeDeadline time.Time
	readDeadline  time.Time
}

func (c *shortWriteConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return c.Conn.Write(p[:1])
}

func (c *shortWriteConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
}

func (c *shortWriteConn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return c.Conn.SetReadDeadline(t)
}

func TestSendBatchRetriesShortWritesToCompletion(t *testing.T) {
	server := newCountingTCPListener(t)

	raw, err := net.Dial("tcp", server.addr())
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	p := newTestPlugin(t, Config{Host: "unused", Port: 1, Timeout: 1000})
	conn := &shortWriteConn{Conn: raw}
	p.connMu.Lock()
	p.conn = conn
	p.connMu.Unlock()

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "short"}}, 1); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	want := `{"route_id":"short"}`
	waitForTCP(t, func() bool { return server.payload(0) == want })
	if got := server.payload(0); got != want {
		t.Fatalf("received payload = %q, want full %q despite short writes", got, want)
	}
	if conn.writeDeadline.IsZero() {
		t.Fatal("write deadline was not set per send")
	}
	if conn.readDeadline.IsZero() {
		t.Fatal("read deadline was not set per send")
	}
}

func TestStopClosesActiveConnection(t *testing.T) {
	server := newCountingTCPListener(t)
	host, port := splitAddr(t, server.addr())

	p := newTestPlugin(t, Config{Host: host, Port: mustAtoi(t, port), Timeout: 1000})
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	p.connMu.Lock()
	conn := p.conn
	p.connMu.Unlock()
	if conn != nil {
		t.Fatal("Stop() left the active connection open")
	}
}

func startTCPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received
}

func startTLSServer(t *testing.T) (string, <-chan string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	serverNames := make(chan string, 1)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- info.ServerName
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go acceptMessage(ln, received)
	return ln.Addr().String(), received, serverNames
}

func acceptMessage(ln net.Listener, received chan<- string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err == nil {
		received <- string(buf[:n])
	}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "logs.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"logs.example.test"},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func splitAddr(t *testing.T, addr string) (string, string) {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return host, port
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var out int
	if _, err := fmt.Sscanf(value, "%d", &out); err != nil {
		t.Fatalf("parse int %q: %v", value, err)
	}
	return out
}

func waitForTCPPayload(t *testing.T, received <-chan string) map[string]any {
	t.Helper()
	select {
	case message := <-received:
		var payload map[string]any
		if err := json.Unmarshal([]byte(message), &payload); err != nil {
			t.Fatalf("unmarshal TCP payload: %v", err)
		}
		return payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TCP payload")
		return nil
	}
}

func assertNestedField(t *testing.T, payload map[string]any, object, field string, want any) {
	t.Helper()
	nested, ok := payload[object].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", object, payload[object])
	}
	if got := nested[field]; got != want {
		t.Fatalf("%s.%s = %#v, want %#v", object, field, got, want)
	}
}

func assertNestedNonnegativeNumber(t *testing.T, payload map[string]any, object, field string) {
	t.Helper()
	nested, ok := payload[object].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", object, payload[object])
	}
	value, ok := nested[field].(float64)
	if !ok || value < 0 {
		t.Fatalf("%s.%s = %#v, want nonnegative number", object, field, nested[field])
	}
}

func TestSendMarshalErrorNamesTCPLogger(t *testing.T) {
	entries := make(chan logger.Entry, 1)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if entry.Level == "ERROR" {
			entries <- entry
		}
	})
	t.Cleanup(stop)

	(&Plugin{}).Send(map[string]any{"unsupported": make(chan int)})

	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, "in tcp-logger") {
			t.Fatalf("marshal error message = %q, want tcp-logger", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for tcp-logger marshal error")
	}
}
