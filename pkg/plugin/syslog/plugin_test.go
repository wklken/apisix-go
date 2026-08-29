package syslog

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
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
				"syslog-security-warning-"+test.name,
				func(entry logger.Entry) {
					if entry.Level == "WARN" &&
						strings.Contains(entry.Message, "tls disabled in syslog") {
						warnings = append(warnings, entry.Message)
					}
				},
			)
			defer stop()

			p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 5140, TLS: test.tls})
			t.Cleanup(p.Stop)

			if test.wantWarn {
				if len(warnings) != 1 ||
					warnings[0] != "Keeping tls disabled in syslog configuration is a security risk" {
					t.Fatalf("warnings = %#v, want exact disabled TLS warning", warnings)
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %#v, want none with TLS enabled", warnings)
			}
		})
	}
}

func TestRunLogPhaseEnqueuesRFC5424FrameWithDetachedFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{}
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet, URI: "/orders", Host: "gateway.example:8443",
			RemoteAddr: "192.0.2.8:9000",
		},
		Response: apisixlog.ResponseLogSnapshot{Header: http.Header{"X-Test": {"ok"}}},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusCreated, Bytes: 7},
		Started:  time.Unix(10, 0), Finished: time.Unix(11, 0),
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		frame, ok := entry[syslogFrameKey].([]byte)
		if !ok || !bytes.HasPrefix(frame, []byte("<46>1 ")) {
			t.Fatalf("frame = %q", frame)
		}
		_, payload, ok := bytes.Cut(frame, []byte(" - - "))
		if !ok {
			t.Fatalf("RFC5424 frame missing structured header: %q", frame)
		}
		var fields map[string]any
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatalf("decode frame payload: %v", err)
		}
		response, _ := fields["response"].(map[string]any)
		if response["status"] != float64(http.StatusCreated) {
			t.Fatalf("response fields = %#v", fields["response"])
		}
	case <-time.After(time.Second):
		t.Fatal("detached syslog frame was not delivered")
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
	config := Config{Host: "127.0.0.1", Port: 514}
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
	firstNested, firstNestedOK := first.SnapshotLogFormat["nested"].(map[string]any)
	secondNested, secondNestedOK := second.SnapshotLogFormat["nested"].(map[string]any)
	if !firstNestedOK || !secondNestedOK {
		t.Fatalf(
			"custom snapshot metadata = %#v/%#v",
			first.SnapshotLogFormat,
			second.SnapshotLogFormat,
		)
	}
	if firstNested["generation"] != "n" || first.config.MaxPendingEntries != 11 ||
		!first.customLogFormat {
		t.Fatalf(
			"generation N custom metadata = %#v/%d/%v",
			firstNested,
			first.config.MaxPendingEntries,
			first.customLogFormat,
		)
	}
	if secondNested["generation"] != "n-plus-one" || second.config.MaxPendingEntries != 12 ||
		!second.customLogFormat {
		t.Fatalf(
			"generation N+1 custom metadata = %#v/%d/%v",
			secondNested,
			second.config.MaxPendingEntries,
			second.customLogFormat,
		)
	}

	firstExtra := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format_extra": map[string]any{
			"nested": map[string]any{"generation": "n-extra"},
		},
		"max_pending_entries": 11,
	})
	secondExtra := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format_extra": map[string]any{
			"nested": map[string]any{"generation": "n-plus-one-extra"},
		},
		"max_pending_entries": 12,
	})
	firstExtraNested, firstExtraNestedOK := firstExtra.SnapshotLogFormatExtra["nested"].(map[string]any)
	secondExtraNested, secondExtraNestedOK := secondExtra.SnapshotLogFormatExtra["nested"].(map[string]any)
	if !firstExtraNestedOK || !secondExtraNestedOK {
		t.Fatalf(
			"extra snapshot metadata = %#v/%#v",
			firstExtra.SnapshotLogFormatExtra,
			secondExtra.SnapshotLogFormatExtra,
		)
	}
	if firstExtraNested["generation"] != "n-extra" || firstExtra.customLogFormat {
		t.Fatalf(
			"generation N extra metadata = %#v/%v",
			firstExtraNested,
			firstExtra.customLogFormat,
		)
	}
	if secondExtraNested["generation"] != "n-plus-one-extra" || secondExtra.customLogFormat {
		t.Fatalf(
			"generation N+1 extra metadata = %#v/%v",
			secondExtraNested,
			secondExtra.customLogFormat,
		)
	}
}

func TestMetadataDecodeFailsBeforeSyslogTransportAndProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{Host: "127.0.0.1", Port: 514}}
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
	if p.transport != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"decode failure acquired syslog resources: transport=%v processor=%v",
			p.transport,
			p.BatchProcessor,
		)
	}
}

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{Host: "127.0.0.1", Port: 514})

	if p.config.Timeout != 3000 {
		t.Fatalf("timeout = %d, want official default 3000 milliseconds", p.config.Timeout)
	}
	if p.config.FlushLimit != 4096 {
		t.Fatalf("flush_limit = %d, want 4096", p.config.FlushLimit)
	}
	if p.config.DropLimit != 1048576 {
		t.Fatalf("drop_limit = %d, want 1048576", p.config.DropLimit)
	}
	if p.config.PoolSize != 5 {
		t.Fatalf("pool_size = %d, want 5", p.config.PoolSize)
	}
	if p.config.SockType != "tcp" {
		t.Fatalf("sock_type = %q, want tcp", p.config.SockType)
	}
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

func TestEncodeRFC5424UsesSyslogInfoEnvelope(t *testing.T) {
	timestamp := time.Date(2026, time.July, 28, 6, 7, 8, 987654321, time.UTC)
	got := encodeRFC5424(timestamp, "gateway.example", 4242, []byte(`{"path":"/orders"}`))
	want := "<46>1 2026-07-28T06:07:08.987Z gateway.example apisix 4242 - - " +
		"{\"path\":\"/orders\"}\n"
	if string(got) != want {
		t.Fatalf("encodeRFC5424() = %q, want %q", got, want)
	}
}

func TestJoinRFC5424FramesPreservesBatchOrdering(t *testing.T) {
	first := []byte("<46>1 first\n")
	second := []byte("<46>1 second\n")
	got := joinRFC5424Frames([][]byte{first, second})
	if string(got) != "<46>1 first\n<46>1 second\n" {
		t.Fatalf("joinRFC5424Frames() = %q, want concatenated frames", got)
	}
}

func TestSendWritesUDPMessage(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		SockType:   "udp",
		Timeout:    3000,
		FlushLimit: 1,
		LogFormat:  map[string]any{"path": "$uri"},
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		assertDirectRFC5424Frame(t, message, `{"path":"/orders"}`)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestSendWritesTCPMessage(t *testing.T) {
	addr, received := startTCPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tcp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		Timeout:    3000,
		FlushLimit: 1,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		assertDirectRFC5424Frame(t, message, `{"path":"/orders"}`)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog TCP message")
	}
}

func TestSendWritesTLSMessage(t *testing.T) {
	addr, received, serverNames := startTLSServer(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	var config Config
	if err := util.Parse(map[string]any{
		"host":        "localhost",
		"port":        mustAtoi(t, port),
		"timeout":     3000,
		"flush_limit": 1,
		"sock_type":   "tcp",
		"tls":         true,
		"ssl_verify":  false,
	}, &config); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, config)
	p.Send(map[string]any{"path": "/secure"})

	select {
	case message := <-received:
		assertDirectRFC5424Frame(t, message, `{"path":"/secure"}`)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog TLS message")
	}

	select {
	case got := <-serverNames:
		if got != "localhost" {
			t.Fatalf("SNI = %q, want configured host localhost", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog TLS server name")
	}
}

func TestSendRejectsUntrustedTLSMessageByDefault(t *testing.T) {
	addr, _, _ := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		Timeout:    3000,
		FlushLimit: 1,
		SockType:   "tcp",
		TLS:        true,
	})
	if err := p.sendBody(context.Background(), []byte("secure")); err == nil {
		t.Fatal("sendBody() error = nil, want untrusted TLS peer rejection")
	}
}

func TestTLSHandshakeHonorsConfiguredTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	p := newTestPlugin(t, Config{
		Host:       host,
		Port:       mustAtoi(t, port),
		Timeout:    25,
		FlushLimit: 1,
		SockType:   "tcp",
		TLS:        true,
	})

	started := time.Now()
	err = p.sendBody(context.Background(), []byte("frame"))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("sendBody() error = nil, want TLS handshake timeout")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("sendBody() elapsed = %s, want configured timeout to bound handshake", elapsed)
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("server did not accept TLS connection")
	}
}

func TestStopIsBoundedWhenSyslogSinkStopsReading(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr: %v", err)
	}
	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		Timeout:      25,
		FlushLimit:   1,
		DropLimit:    64 << 20,
		BatchMaxSize: 1,
	})
	p.BatchProcessor.Push(map[string]any{
		syslogFrameKey: bytes.Repeat([]byte("x"), 32<<20),
	})

	var connection net.Conn
	select {
	case connection = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("server did not accept syslog connection")
	}

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
		_ = connection.Close()
	case <-time.After(500 * time.Millisecond):
		_ = connection.Close()
		<-stopped
		t.Fatal("Plugin.Stop() blocked after configured write timeout")
	}
	if stats := p.transport.Stats(); stats.Buffered != 0 {
		t.Fatal("Plugin.Stop() retained an orphan suffix after an ambiguous partial write")
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

func TestHandlerBatchesSyslogMessages(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		SockType:        "udp",
		Timeout:         3000,
		FlushLimit:      1,
		BatchMaxSize:    2,
		InactiveTimeout: 60,
		BufferDuration:  60,
		LogFormat:       map[string]any{"path": "$uri"},
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
		frames := splitRFC5424Frames(t, message)
		if len(frames) != 2 {
			t.Fatalf("batch frames = %d, want 2: %q", len(frames), message)
		}
		first := extractJSONPayload(t, frames[0])
		second := extractJSONPayload(t, frames[1])
		if first["path"] != "/one" || second["path"] != "/two" {
			t.Fatalf(
				"batch paths = %#v then %#v, want /one then /two",
				first["path"],
				second["path"],
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP batch message")
	}
}

func TestHandlerManualFlushDeliversBufferedFrame(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		SockType:        "udp",
		Timeout:         3000,
		BatchMaxSize:    10,
		InactiveTimeout: 60,
		BufferDuration:  60,
		LogFormat:       map[string]any{"path": "$uri"},
	})

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/manual", nil))
	p.Stop()

	select {
	case message := <-received:
		frames := splitRFC5424Frames(t, message)
		if len(frames) != 1 {
			t.Fatalf("manual flush frames = %d, want 1: %q", len(frames), message)
		}
		payload := extractJSONPayload(t, frames[0])
		if payload["path"] != "/manual" {
			t.Fatalf("manual flush path = %#v, want /manual", payload["path"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for manually flushed syslog frame")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		SockType:         "udp",
		Timeout:          3000,
		FlushLimit:       1,
		BatchMaxSize:     1,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":1}`),
	)
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

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
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
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerDefaultLogContainsLatencyAndUpstream(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
	})
	p.SetRouteContext("syslog-default", "127.0.0.1:9080")

	req := httptest.NewRequest(http.MethodGet, "http://gateway.example/orders", nil)
	req.Host = "gateway.example"
	req.RemoteAddr = "192.0.2.20:54321"
	req = apisixctx.WithApisixVars(req, map[string]string{})
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "198.51.100.30")
		apisixctx.RegisterApisixVar(r, "$balancer_port", "1980")
		apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(1))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})).ServeHTTP(rr, req)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if _, ok := payload["apisix_latency"].(float64); !ok {
			t.Fatalf("apisix_latency = %#v, want numeric milliseconds", payload["apisix_latency"])
		}
		if payload["upstream"] != "198.51.100.30:1980" {
			t.Fatalf("upstream = %#v, want selected upstream", payload["upstream"])
		}
		request, ok := payload["request"].(map[string]any)
		if !ok || request["method"] != http.MethodGet {
			t.Fatalf("request = %#v, want GET request object", payload["request"])
		}
		response, ok := payload["response"].(map[string]any)
		if !ok || response["status"] != float64(http.StatusCreated) {
			t.Fatalf("response = %#v, want status 201", payload["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rich syslog record")
	}
}

func TestHandlerLogFormatExtraEnrichesDefaultWithoutClobbering(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:           host,
		Port:           mustAtoi(t, port),
		SockType:       "udp",
		Timeout:        3000,
		FlushLimit:     1,
		BatchMaxSize:   1,
		LogFormatExtra: map[string]any{"marker": "extra", "route_id": "wrong"},
	})
	p.RouteID = "route-1"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/extra", nil))

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if payload["marker"] != "extra" {
			t.Fatalf("payload marker = %#v, want extra", payload["marker"])
		}
		if payload["route_id"] != "route-1" {
			t.Fatalf("payload route_id = %#v, want default field preserved", payload["route_id"])
		}
		if _, ok := payload["apisix_latency"]; !ok {
			t.Fatalf("payload = %#v, want default apisix_latency", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerExplicitEmptyLogFormatUsesCustomMode(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
		LogFormat:    map[string]any{},
		logFormatSet: true,
	})
	p.RouteID = "route-empty-format"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/empty", nil))

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if len(payload) != 1 || payload["route_id"] != "route-empty-format" {
			t.Fatalf("payload = %#v, want custom-format runtime route_id only", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerRemovesStaleServiceIDWithoutRuntimeService(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"marker":     "custom",
			"service_id": "stale",
		},
	})
	p.RouteID = "route-no-service"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/no-service", nil))

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if _, ok := payload["service_id"]; ok {
			t.Fatalf(
				"payload service_id = %#v, want absent without runtime service",
				payload["service_id"],
			)
		}
		if payload["route_id"] != "route-no-service" {
			t.Fatalf("payload route_id = %#v, want route-no-service", payload["route_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerCustomLogFormatIncludesRuntimeServiceID(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
		LogFormat:    map[string]any{"marker": "custom"},
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/with-service", nil)
	request = apisixctx.WithApisixVars(request, map[string]string{})
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$service_id", "service-1")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if payload["service_id"] != "service-1" {
			t.Fatalf("payload service_id = %#v, want service-1", payload["service_id"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerResolvesNestedFormatVariablesAndConstants(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"nested": map[string]any{
				"host": "$host",
				"constant": map[string]any{
					"number": 7,
					"bool":   true,
				},
			},
		},
	})
	p.RouteID = "route-nested-format"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://nested.example/path", nil))

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		nested, ok := payload["nested"].(map[string]any)
		if !ok {
			t.Fatalf("payload nested = %#v, want object", payload["nested"])
		}
		if nested["host"] != "nested.example" {
			t.Fatalf("nested host = %#v, want nested.example", nested["host"])
		}
		constant, ok := nested["constant"].(map[string]any)
		if !ok || constant["number"] != float64(7) || constant["bool"] != true {
			t.Fatalf("nested constant = %#v, want number and bool preserved", nested["constant"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerTruncatesLogFormatAtPinnedDepth(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:         host,
		Port:         mustAtoi(t, port),
		SockType:     "udp",
		Timeout:      3000,
		FlushLimit:   1,
		BatchMaxSize: 1,
		LogFormat: map[string]any{
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{
						"d": map[string]any{
							"e": map[string]any{
								"f": map[string]any{"g": "$host"},
							},
						},
					},
				},
			},
		},
	})
	p.RouteID = "route-depth"
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://depth.example/path", nil))

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		current := payload
		for _, key := range []string{"a", "b", "c", "d"} {
			next, ok := current[key].(map[string]any)
			if !ok {
				t.Fatalf("payload path %s = %#v, want object", key, current[key])
			}
			current = next
		}
		e, ok := current["e"].(map[string]any)
		if !ok || len(e) != 0 {
			t.Fatalf("depth-five value = %#v, want truncated empty object", current["e"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		SockType:            "udp",
		Timeout:             3000,
		FlushLimit:          1,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":2}`),
	)
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
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
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	addr, received := startUDPServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split udp addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		SockType:            "udp",
		Timeout:             3000,
		FlushLimit:          1,
		BatchMaxSize:        1,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":3}`),
	)
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

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("payload request body = %#v, want absent", request["body"])
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("payload response body = %#v, want absent", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for syslog UDP message")
	}
}

func TestSchemaAcceptsOfficialBodyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                   "127.0.0.1",
		"port":                   514,
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
		"max_req_body_bytes":     1024,
		"max_resp_body_bytes":    2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body fields: %v", err)
	}
}

func TestSchemaDiagnosticsMatchPinnedSource(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{"host": "127.0.0.1"}, p.GetSchema())
	if err == nil || !strings.Contains(err.Error(), `missing properties: 'port'`) {
		t.Fatalf("missing port error = %v, want pinned diagnostic", err)
	}
	err = util.Validate(map[string]any{"host": "127.0.0.1", "port": "514"}, p.GetSchema())
	if err == nil || !strings.Contains(err.Error(), "expected integer, but got string") {
		t.Fatalf("string port error = %v, want equivalent typed-port diagnostic", err)
	}
}

func TestSchemaAcceptsOfficialBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                "127.0.0.1",
		"port":                514,
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
		"port":       514,
		"ssl_verify": false,
	}, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected ssl_verify: %v", err)
	}
}

func TestSchemaAcceptsLogFormatExtraAndExposesMetadataSchema(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{
		"host":             "127.0.0.1",
		"port":             514,
		"log_format_extra": map[string]any{"cluster": "$host"},
	}, p.GetSchema()); err != nil {
		t.Fatalf("route schema rejected log_format_extra: %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}
	if err := util.Validate(map[string]any{
		"log_format":       map[string]any{"host": "$host"},
		"log_format_extra": map[string]any{"cluster": "east"},
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected official log formats: %v", err)
	}
}

func TestSchemasRejectStringLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for name, test := range map[string]struct {
		config map[string]any
		schema string
	}{
		"route": {
			config: map[string]any{
				"host":       "127.0.0.1",
				"port":       514,
				"log_format": "$host",
			},
			schema: p.GetSchema(),
		},
		"metadata": {
			config: map[string]any{"log_format": "$host"},
			schema: p.GetMetadataSchema(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := util.Validate(test.config, test.schema)
			if err == nil || !strings.Contains(err.Error(), "expected object") {
				t.Fatalf("schema error = %v, want object validation error", err)
			}
		})
	}
}

func TestPluginMetadataUnmarshalPreservesNestedFormatsAndConstants(t *testing.T) {
	var metadata pluginMetadata
	if err := json.Unmarshal([]byte(`{
		"log_format": {
			"nested": {
				"host": "$host",
				"number": 7,
				"bool": true
			}
		}
	}`), &metadata); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	nested, ok := metadata.LogFormat["nested"].(map[string]any)
	if !ok {
		t.Fatalf("metadata nested format = %#v, want object", metadata.LogFormat["nested"])
	}
	if nested["host"] != "$host" || nested["number"] != float64(7) || nested["bool"] != true {
		t.Fatalf("metadata nested format = %#v, want variable, number, and bool preserved", nested)
	}
}

func TestSelectLogFormatsMatchesRouteAndMetadataPrecedence(t *testing.T) {
	metadata := pluginMetadata{
		LogFormat:      map[string]any{"source": "metadata"},
		LogFormatExtra: map[string]any{"extra": "metadata"},
	}

	format, extra := selectLogFormats(Config{}, metadata)
	if format["source"] != "metadata" || len(extra) != 0 {
		t.Fatalf("metadata selection = %#v/%#v, want metadata log_format only", format, extra)
	}

	format, extra = selectLogFormats(Config{
		LogFormat:      map[string]any{"source": "route"},
		LogFormatExtra: map[string]any{"ignored": "route"},
	}, metadata)
	if format["source"] != "route" || len(extra) != 0 {
		t.Fatalf("route selection = %#v/%#v, want route log_format only", format, extra)
	}

	format, extra = selectLogFormats(Config{
		LogFormatExtra: map[string]any{"extra": "route"},
	}, pluginMetadata{LogFormatExtra: map[string]any{"extra": "metadata", "stale": "metadata"}})
	if len(format) != 0 || extra["extra"] != "route" {
		t.Fatalf("route extra selection = %#v/%#v, want route extra", format, extra)
	}
	if _, ok := extra["stale"]; ok {
		t.Fatalf("route extra selection = %#v, want metadata extra replaced", extra)
	}
}

func extractJSONPayload(t *testing.T, message string) map[string]any {
	t.Helper()

	start := strings.Index(message, "{")
	end := strings.LastIndex(message, "}")
	if start == -1 || end == -1 || end < start {
		t.Fatalf("message = %q, want JSON payload", message)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(message[start:end+1]), &payload); err != nil {
		t.Fatalf("unmarshal syslog payload: %v", err)
	}
	return payload
}

func splitRFC5424Frames(t *testing.T, message string) []string {
	t.Helper()

	if !strings.HasSuffix(message, "\n") {
		t.Fatalf("message = %q, want newline-terminated RFC5424 frame", message)
	}
	lines := strings.Split(strings.TrimSuffix(message, "\n"), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "<46>1 ") {
			t.Fatalf("frame = %q, want RFC5424 SYSLOG/INFO prefix", line)
		}
	}
	return lines
}

func startUDPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve udp addr: %v", err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	received := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _, err := conn.ReadFromUDP(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	return conn.LocalAddr().String(), received
}

func startTCPServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan string, 1)
	go acceptMessage(listener, received)
	return listener.Addr().String(), received
}

func startTLSServer(t *testing.T) (string, <-chan string, <-chan string) {
	t.Helper()

	serverNames := make(chan string, 1)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{testCertificate(t)},
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			serverNames <- info.ServerName
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan string, 1)
	go acceptMessage(listener, received)
	return listener.Addr().String(), received, serverNames
}

func acceptMessage(listener net.Listener, received chan<- string) {
	connection, err := listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = connection.Close() }()

	_ = connection.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buffer := make([]byte, 4096)
	var message []byte
	for {
		count, readErr := connection.Read(buffer)
		if count > 0 {
			message = append(message, buffer[:count]...)
		}
		if readErr != nil {
			if len(message) > 0 {
				received <- string(message)
			}
			return
		}
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
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		&template,
		&template,
		&key.PublicKey,
		key,
	)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certificate
}

func assertDirectRFC5424Frame(t *testing.T, message, body string) {
	t.Helper()

	pattern := `^<46>1 [0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z - apisix [0-9]+ - - ` +
		regexp.QuoteMeta(
			body,
		) + `\n$`
	if !regexp.MustCompile(pattern).MatchString(message) {
		t.Fatalf("message = %q, want RFC5424 frame matching %q", message, pattern)
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestUnmarshalJSONPreservesExplicitFieldPresence(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{
		"host": "127.0.0.1",
		"port": 514,
		"retry_delay": 0,
		"log_format": {"method": "$request_method"},
		"log_format_extra": {"host": "$host"}
	}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if cfg.RetryDelay != 0 || !cfg.retryDelaySet {
		t.Fatalf(
			"retry_delay = %d, set = %v, want explicit zero preserved",
			cfg.RetryDelay,
			cfg.retryDelaySet,
		)
	}
	if cfg.LogFormat["method"] != "$request_method" || !cfg.logFormatSet {
		t.Fatalf(
			"log_format = %#v, set = %v, want decoded and present",
			cfg.LogFormat,
			cfg.logFormatSet,
		)
	}
	if cfg.LogFormatExtra["host"] != "$host" || !cfg.logFormatExtraSet {
		t.Fatalf(
			"log_format_extra = %#v, set = %v, want decoded and present",
			cfg.LogFormatExtra,
			cfg.logFormatExtraSet,
		)
	}
}

func TestUnmarshalJSONDetectsAbsentFields(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"host": "127.0.0.1", "port": 514}`), &cfg); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if cfg.retryDelaySet || cfg.logFormatSet || cfg.logFormatExtraSet {
		t.Fatalf("absent fields reported present: retry_delay=%v log_format=%v log_format_extra=%v",
			cfg.retryDelaySet, cfg.logFormatSet, cfg.logFormatExtraSet)
	}
}
