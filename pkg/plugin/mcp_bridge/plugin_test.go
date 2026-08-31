package mcp_bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

type captureWriteCloser struct {
	bytes.Buffer
}

func (*captureWriteCloser) Close() error { return nil }

func TestSchemaMatchesAPISIX317SanityMatrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	wantFields := map[string]struct{}{"base_uri": {}, "command": {}, "args": {}}
	if len(document.Properties) != len(wantFields) {
		t.Fatalf("schema properties = %v, want only APISIX 3.17 fields", document.Properties)
	}
	for field := range wantFields {
		if _, ok := document.Properties[field]; !ok {
			t.Fatalf("schema is missing APISIX 3.17 field %q", field)
		}
	}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{name: "command", config: map[string]any{"command": "npx"}},
		{name: "missing command", config: map[string]any{}, wantErr: true},
		{name: "numeric command", config: map[string]any{"command": 123}, wantErr: true},
		{name: "string args", config: map[string]any{"command": "npx", "args": []any{"-y", "test"}}},
		{name: "scalar args", config: map[string]any{"command": "npx", "args": "test"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want APISIX 3.17 schema rejection")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want APISIX 3.17 schema acceptance", err)
			}
		})
	}
}

func TestNewSessionIDReturnsErrorForFailingReader(t *testing.T) {
	id, err := newSessionID(failingReader{})
	if err == nil {
		t.Fatal("newSessionID() error = nil, want failure")
	}
	if id != "" {
		t.Fatalf("newSessionID() = %q, want empty on failure", id)
	}
}

func TestRunRequestPhasePublishesAPISIXSourceForUnknownEndpoint(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "sh", Args: []string{"-c", "exit 0"}})
	request := httptest.NewRequest(http.MethodGet, "/mcp/unknown", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("result = %+v, want apisix stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("source = %q, want apisix", lifecycle.ResponseSource())
	}
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.closeAll)

	return p
}

func TestSSEStartsProcessAndAdvertisesMessageEndpoint(t *testing.T) {
	p := newTestPlugin(t, Config{
		BaseURI: "/mcp",
		Command: "sh",
		Args:    []string{"-c", `printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'`},
	})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/mcp/sse")
	if err != nil {
		t.Fatalf("GET /mcp/sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	reader := bufio.NewReader(resp.Body)
	event, data := readSSEEvent(t, reader)
	if event != "endpoint" {
		t.Fatalf("first event = %q, want endpoint", event)
	}
	endpoint, err := url.Parse(data)
	if err != nil {
		t.Fatalf("parse endpoint data %q: %v", data, err)
	}
	if endpoint.Path != "/mcp/message" {
		t.Fatalf("endpoint path = %q, want /mcp/message", endpoint.Path)
	}
	if endpoint.Query().Get("sessionId") == "" {
		t.Fatalf("endpoint data = %q, want sessionId", data)
	}

	event, data = readSSEEvent(t, reader)
	assertPingEvent(t, event, data, "ping:1")

	event, data = readSSEEvent(t, reader)
	if event != "message" {
		t.Fatalf("process event = %q, want message", event)
	}
	if data != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("process data = %q", data)
	}
}

func TestSSECommandStartFailureReturns500WithoutPublishingSession(t *testing.T) {
	p := newTestPlugin(t, Config{Command: filepath.Join(t.TempDir(), "missing-mcp-command")})
	request := httptest.NewRequest(http.MethodGet, "/sse", nil)
	response := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	p.mu.Lock()
	sessions := len(p.sessions)
	p.mu.Unlock()
	if sessions != 0 {
		t.Fatalf("published sessions = %d, want 0 after command start failure", sessions)
	}
}

func TestMessageEndpointWritesToSessionStdin(t *testing.T) {
	p := newTestPlugin(t, Config{
		Command: "sh",
		Args:    []string{"-c", `while IFS= read -r line; do printf '%s\n' "$line"; done`},
	})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	_, endpointData := readSSEEvent(t, reader)
	event, data := readSSEEvent(t, reader)
	assertPingEvent(t, event, data, "ping:1")
	endpoint, err := url.Parse(endpointData)
	if err != nil {
		t.Fatalf("parse endpoint data %q: %v", endpointData, err)
	}

	messageURL := server.URL + endpoint.Path + "?" + endpoint.RawQuery
	postResp, err := http.Post(messageURL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":2}`))
	if err != nil {
		t.Fatalf("POST /message: %v", err)
	}
	defer func() { _ = postResp.Body.Close() }()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("message status = %d, want 202", postResp.StatusCode)
	}

	event, data = readSSEEvent(t, reader)
	if event != "message" {
		t.Fatalf("event = %q, want message", event)
	}
	if data != `{"jsonrpc":"2.0","id":2}` {
		t.Fatalf("data = %q, want posted JSON-RPC body", data)
	}
}

func TestStderrIsForwardedAsMCPNotification(t *testing.T) {
	p := newTestPlugin(t, Config{
		Command: "sh",
		Args:    []string{"-c", `printf 'boom\n' >&2`},
	})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	readSSEEvent(t, reader)
	event, data := readSSEEvent(t, reader)
	assertPingEvent(t, event, data, "ping:1")

	event, data = readSSEEvent(t, reader)
	if event != "message" {
		t.Fatalf("event = %q, want message", event)
	}
	if !strings.Contains(data, `"method":"notifications/stderr"`) || !strings.Contains(data, `"content":"boom"`) {
		t.Fatalf("stderr data = %q", data)
	}
}

func TestMessageEndpointRejectsUnknownSession(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})

	req := httptest.NewRequest(http.MethodPost, "/message?sessionId=missing", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if got := apisixlog.GetField(req, "$request_uri"); got != "/message?sessionId=***" {
		t.Fatalf("logged request URI = %#v, want redacted sessionId", got)
	}
}

func TestMessageEndpointAcceptsLargeBodyWithoutLocalLimit(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})

	stdin := &captureWriteCloser{}
	sess := &session{
		id:        "session",
		stdin:     stdin,
		cancel:    func() {},
		events:    make(chan sseEvent),
		done:      make(chan struct{}),
		tasks:     runtime.NewRequestTaskGroup(context.Background(), "test/mcp-bridge"),
		closeDone: make(chan struct{}),
	}
	p.mu.Lock()
	p.sessions[sess.id] = sess
	p.mu.Unlock()

	body := strings.Repeat("x", 1<<20+1)
	req := httptest.NewRequest(http.MethodPost, "/message?sessionId=session", strings.NewReader(body))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	if got := stdin.String(); got != body+"\n" {
		t.Fatalf("session stdin = %d bytes, want %d bytes", len(got), len(body)+1)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored request body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored request body = %d bytes, want %d bytes", len(restored), len(body))
	}
}

func TestSSEEmitsPeriodicPingRequests(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})
	p.pingInterval = 10 * time.Millisecond
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	reader := bufio.NewReader(resp.Body)
	readSSEEvent(t, reader)

	event, data := readSSEEvent(t, reader)
	assertPingEvent(t, event, data, "ping:1")
	event, data = readSSEEvent(t, reader)
	assertPingEvent(t, event, data, "ping:2")
}

func TestScanPipeStopsWhenSessionIsCanceledWithFullEventQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan sseEvent, 1)
	done := make(chan struct{})
	go func() {
		scanPipe(ctx, strings.NewReader("one\ntwo\nthree\n"), "message", events)
		close(done)
	}()

	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("scanPipe did not fill event queue")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scanPipe did not stop after cancellation")
	}
}

func assertPingEvent(t *testing.T, event, data, id string) {
	t.Helper()
	if event != "message" || !strings.Contains(data, `"jsonrpc":"2.0"`) ||
		!strings.Contains(data, `"method":"ping"`) || !strings.Contains(data, `"id":"`+id+`"`) {
		t.Fatalf("ping event = (%q, %q), want id %q", event, data, id)
	}
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	lines := make(chan []string, 1)
	errs := make(chan error, 1)
	go func() {
		var got []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				errs <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				lines <- got
				return
			}
			got = append(got, line)
		}
	}()

	select {
	case got := <-lines:
		var event, data string
		for _, line := range got {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		return event, data
	case err := <-errs:
		if err == io.EOF {
			t.Fatal("unexpected EOF while reading SSE event")
		}
		t.Fatalf("read SSE event: %v", err)
	case <-deadline:
		t.Fatal("timed out waiting for SSE event")
	}

	return "", ""
}

func TestSSEFastChildEventsAreNeverDropped(t *testing.T) {
	// Regression: the request context can cancel concurrently with a fast
	// child producing its final event. The handler must still deliver the
	// child's buffered events (observed as EOF at the message read under
	// CI load before the drain fix).
	for iteration := range 200 {
		p := newTestPlugin(t, Config{
			BaseURI: "/mcp",
			Command: "sh",
			Args:    []string{"-c", `printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'`},
		})
		server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("next handler should not be called")
		})))
		t.Cleanup(server.Close)

		resp, err := http.Get(server.URL + "/mcp/sse")
		if err != nil {
			t.Fatalf("iteration %d: GET /mcp/sse: %v", iteration, err)
		}
		reader := bufio.NewReader(resp.Body)
		if _, data := readSSEEvent(t, reader); !strings.Contains(data, "sessionId") {
			t.Fatalf("iteration %d: endpoint data = %q", iteration, data)
		}
		event, data := readSSEEvent(t, reader)
		assertPingEvent(t, event, data, "ping:1")
		event, data = readSSEEvent(t, reader)
		if event != "message" {
			t.Fatalf("iteration %d: process event = %q, want message", iteration, event)
		}
		if data != `{"jsonrpc":"2.0","id":1,"result":{}}` {
			t.Fatalf("iteration %d: process data = %q", iteration, data)
		}
		_ = resp.Body.Close()
	}
}

const (
	mcpBridgeChildEnv      = "APISIX_GO_MCP_BRIDGE_CHILD"
	mcpBridgeGateEnv       = "APISIX_GO_MCP_BRIDGE_GATE"
	mcpBridgeChildModeEnv  = "APISIX_GO_MCP_BRIDGE_CHILD_MODE"
	mcpBridgePanicEnv      = "APISIX_GO_MCP_BRIDGE_PANIC_HELPER"
	mcpBridgeGrandchildEnv = "APISIX_GO_MCP_BRIDGE_GRANDCHILD"
)

var mcpBridgeScannerPanic = &struct{ marker string }{marker: "mcp-scanner-panic"}

type gatedSSEWriter struct {
	header       http.Header
	writeStarted chan struct{}
	release      chan struct{}
	startedOnce  sync.Once
}

func newGatedSSEWriter() *gatedSSEWriter {
	return &gatedSSEWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
}

func (w *gatedSSEWriter) Header() http.Header { return w.header }

func (w *gatedSSEWriter) WriteHeader(int) {}

func (w *gatedSSEWriter) Write([]byte) (int, error) {
	w.startedOnce.Do(func() { close(w.writeStarted) })
	<-w.release
	return 0, errors.New("test SSE writer failure")
}

type stagedSSEWriter struct {
	header      http.Header
	mu          sync.Mutex
	buf         bytes.Buffer
	writeCount  int
	blocked     chan struct{}
	release     chan struct{}
	blockedOnce sync.Once
}

func newStagedSSEWriter() *stagedSSEWriter {
	return &stagedSSEWriter{
		header:  make(http.Header),
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *stagedSSEWriter) Header() http.Header { return w.header }

func (w *stagedSSEWriter) WriteHeader(int) {}

func (w *stagedSSEWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.writeCount++
	writeCount := w.writeCount
	w.mu.Unlock()
	if writeCount == 3 {
		w.blockedOnce.Do(func() { close(w.blocked) })
		<-w.release
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(data)
}

func (w *stagedSSEWriter) Flush() {}

func (w *stagedSSEWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestSSEWriterFailureJoinsSession(t *testing.T) {
	gate := newMCPBridgeGate(t)
	p := newTestPlugin(t, mcpBridgeChildConfig(t, "hold"))
	t.Cleanup(func() { releaseMCPBridgeGate(t, gate) })
	writer := newGatedSSEWriter()
	request := httptest.NewRequest(http.MethodGet, "/sse", nil)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		p.handleSSE(writer, request)
	}()

	<-writer.writeStarted
	sess := waitForMCPBridgeSession(t, p)
	waitForMCPBridgeChildrenReady(t, gate, 1)
	close(writer.release)
	waitForMCPBridgeMapEmpty(t, p)
	assertMCPBridgeNotClosed(t, handlerDone, "SSE handler returned before session join")
	assertMCPBridgeNotClosed(t, sess.done, "session done closed before session join")

	releaseMCPBridgeGate(t, gate)
	waitForMCPBridgeClosed(t, sess.done, "session did not finish after writer failure cleanup")
	waitForMCPBridgeClosed(t, handlerDone, "SSE handler did not return after session cleanup")
}

func TestSSERequestCancelDrainsThenJoins(t *testing.T) {
	gate := newMCPBridgeGate(t)
	p := newTestPlugin(t, mcpBridgeChildConfig(t, "events"))
	t.Cleanup(func() { releaseMCPBridgeGate(t, gate) })
	writer := newStagedSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(ctx)
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		p.handleSSE(writer, request)
	}()

	sess := waitForMCPBridgeSession(t, p)
	<-writer.blocked
	waitForMCPBridgeChildrenReady(t, gate, 1)
	bufferDeadline := time.Now().Add(time.Second)
	for len(sess.events) < 2 {
		if time.Now().After(bufferDeadline) {
			t.Fatalf("buffered event count = %d, want 2", len(sess.events))
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	close(writer.release)
	waitForMCPBridgeMapEmpty(t, p)
	assertMCPBridgeNotClosed(t, handlerDone, "SSE handler returned before canceled session join")
	assertMCPBridgeNotClosed(t, sess.done, "session done closed before canceled session join")

	releaseMCPBridgeGate(t, gate)
	waitForMCPBridgeClosed(t, sess.done, "canceled session did not finish after gate release")
	waitForMCPBridgeClosed(t, handlerDone, "SSE handler did not return after canceled session cleanup")

	output := writer.String()
	previous := -1
	for _, event := range []string{"one", "two", "three"} {
		index := strings.Index(output, "data: "+event+"\n")
		if index < 0 {
			t.Fatalf("output = %q, missing buffered event %q", output, event)
		}
		if index <= previous {
			t.Fatalf("output = %q, event %q was not ordered after previous event", output, event)
		}
		previous = index
	}
}

func TestCloseSessionWaitsForScanners(t *testing.T) {
	gate := newMCPBridgeGate(t)
	p := newTestPlugin(t, mcpBridgeChildConfig(t, "hold"))
	t.Cleanup(func() { releaseMCPBridgeGate(t, gate) })
	sess, err := p.startSession(context.Background())
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	waitForMCPBridgeChildrenReady(t, gate, 1)

	const closers = 2
	closeDone := make(chan struct{}, closers)
	for range closers {
		go func() {
			closeMCPSessionForTest(p, sess)
			closeDone <- struct{}{}
		}()
	}
	waitForMCPBridgeMapEmpty(t, p)
	for range closers {
		assertMCPBridgeNotClosed(t, closeDone, "duplicate close returned before scanner join")
	}
	assertMCPBridgeNotClosed(t, sess.done, "direct close returned before session join")

	releaseMCPBridgeGate(t, gate)
	for range closers {
		waitForMCPBridgeClosed(t, closeDone, "duplicate close did not return after session cleanup")
	}
	waitForMCPBridgeClosed(t, sess.done, "direct close did not finish session cleanup")
}

func TestCloseAllJoinsOutsideLock(t *testing.T) {
	gate := newMCPBridgeGate(t)
	p := newTestPlugin(t, mcpBridgeChildConfig(t, "hold"))
	t.Cleanup(func() { releaseMCPBridgeGate(t, gate) })
	var sessions []*session
	for range 2 {
		sess, err := p.startSession(context.Background())
		if err != nil {
			t.Fatalf("startSession() error = %v", err)
		}
		sessions = append(sessions, sess)
	}
	waitForMCPBridgeChildrenReady(t, gate, len(sessions))
	waitForMCPBridgeSessionCount(t, p, len(sessions))

	closeAllDone := make(chan struct{})
	go func() {
		p.closeAll()
		close(closeAllDone)
	}()
	waitForMCPBridgeMapEmpty(t, p)
	lookupDone := make(chan struct{})
	go func() {
		_ = p.lookupSession("missing")
		close(lookupDone)
	}()
	waitForMCPBridgeClosed(t, lookupDone, "session map mutex remained held while closeAll joined")
	assertMCPBridgeNotClosed(t, closeAllDone, "closeAll returned before joining sessions")
	for _, sess := range sessions {
		assertMCPBridgeNotClosed(t, sess.done, "closeAll returned before a session joined")
	}

	releaseMCPBridgeGate(t, gate)
	waitForMCPBridgeClosed(t, closeAllDone, "closeAll did not return after joining sessions")
	for _, sess := range sessions {
		waitForMCPBridgeClosed(t, sess.done, "closeAll did not finish session cleanup")
	}
}

func TestStartSessionDoesNotPublishBeforeTaskAdmission(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})
	previousAdmission := admitSessionTask
	admissionStarted := make(chan struct{})
	releaseAdmission := make(chan struct{})
	var first sync.Once
	admitSessionTask = func(tasks *runtime.RequestTaskGroup, run func(context.Context) error) error {
		first.Do(func() {
			close(admissionStarted)
			<-releaseAdmission
		})
		return previousAdmission(tasks, run)
	}
	defer func() { admitSessionTask = previousAdmission }()

	type startResult struct {
		sess *session
		err  error
	}
	result := make(chan startResult, 1)
	go func() {
		sess, err := p.startSession(context.Background())
		result <- startResult{sess: sess, err: err}
	}()
	<-admissionStarted
	p.mu.Lock()
	visible := len(p.sessions)
	p.mu.Unlock()
	close(releaseAdmission)

	started := <-result
	if started.err != nil {
		t.Fatalf("startSession() error = %v", started.err)
	}
	p.closeSession(started.sess)
	if visible != 0 {
		t.Fatalf("session map exposed %d session before task admission completed", visible)
	}
}

func TestScannerPanicReturnsFromSessionOwner(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestScannerPanicReturnsFromSessionOwnerHelper$")
	command.Env = append(os.Environ(), mcpBridgePanicEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("scanner panic escaped owner (err %v): %s", err, output)
	}
	markers := []string{
		"mcp-session-map-removed",
		"mcp-session-channels-closed",
		"mcp-session-owner-recovered",
	}
	previous := -1
	for _, marker := range markers {
		index := bytes.Index(output, []byte(marker))
		if index < 0 {
			t.Fatalf("helper output = %s, missing marker %q", output, marker)
		}
		if index <= previous {
			t.Fatalf("helper output = %s, marker %q was emitted before cleanup predecessor", output, marker)
		}
		previous = index
	}
}

func TestScannerPanicReturnsFromSessionOwnerHelper(t *testing.T) {
	if os.Getenv(mcpBridgePanicEnv) != "1" {
		return
	}

	p := newTestPlugin(t, Config{Command: "cat"})
	previousScanner := scanPipeForSession
	scanPipeForSession = func(context.Context, io.Reader, string, chan<- sseEvent) {
		panic(mcpBridgeScannerPanic)
	}
	defer func() { scanPipeForSession = previousScanner }()

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/sse", nil).WithContext(ctx)
	handlerResult := make(chan any, 1)
	go func() {
		defer func() { handlerResult <- recover() }()
		p.handleSSE(httptest.NewRecorder(), request)
	}()
	sess := waitForMCPBridgeSession(t, p)
	cancel()
	recovered := <-handlerResult
	if recovered != mcpBridgeScannerPanic {
		t.Fatalf("recovered panic = %#v, want exact %#v", recovered, mcpBridgeScannerPanic)
	}
	waitForMCPBridgeMapEmpty(t, p)
	fmt.Println("mcp-session-map-removed")
	waitForMCPBridgeClosed(t, sess.done, "scanner panic owner returned before session done")
	fmt.Println("mcp-session-channels-closed")
	if _, ok := <-sess.events; ok {
		t.Fatal("session events channel remained open after scanner panic")
	}
	fmt.Println("mcp-session-owner-recovered")
}

func TestMCPBridgeChild(t *testing.T) {
	if os.Getenv(mcpBridgeChildEnv) != "1" {
		return
	}

	grandchild := exec.Command(os.Args[0], "-test.run=^TestMCPBridgeGrandchild$")
	grandchild.Env = append(os.Environ(), mcpBridgeGrandchildEnv+"=1")
	grandchild.Stdout = os.Stdout
	grandchild.Stderr = os.Stderr
	if err := grandchild.Start(); err != nil {
		os.Exit(2)
	}
	if gate := os.Getenv(mcpBridgeGateEnv); gate != "" {
		pid := fmt.Sprint(os.Getpid())
		_ = os.WriteFile(gate+".ready."+pid, []byte("ready"), 0o600)
	}
	mode := os.Getenv(mcpBridgeChildModeEnv)
	switch mode {
	case "events":
		for _, event := range []string{"one", "two", "three"} {
			_, _ = fmt.Fprintln(os.Stdout, event)
		}
	}
	_, _ = io.ReadAll(os.Stdin)
	waitForMCPBridgeChildGate()
}

func TestMCPBridgeGrandchild(t *testing.T) {
	if os.Getenv(mcpBridgeGrandchildEnv) != "1" {
		return
	}
	waitForMCPBridgeChildGate()
}

func mcpBridgeChildConfig(t *testing.T, mode string) Config {
	t.Helper()
	t.Setenv(mcpBridgeChildEnv, "1")
	t.Setenv(mcpBridgeChildModeEnv, mode)
	return Config{Command: os.Args[0], Args: []string{"-test.run=^TestMCPBridgeChild$"}}
}

func newMCPBridgeGate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release")
	t.Setenv(mcpBridgeGateEnv, path)
	return path
}

func waitForMCPBridgeChildrenReady(t *testing.T, gate string, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ready, err := filepath.Glob(gate + ".ready.*")
		if err == nil && len(ready) >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ready child count = %d, want %d", len(ready), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func releaseMCPBridgeGate(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("release"), 0o600); err != nil {
		t.Fatalf("release MCP bridge gate: %v", err)
	}
}

func waitForMCPBridgeChildGate() {
	path := os.Getenv(mcpBridgeGateEnv)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			os.Exit(2)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForMCPBridgeSession(t *testing.T, p *Plugin) *session {
	t.Helper()
	sessions := waitForMCPBridgeSessionCount(t, p, 1)
	return sessions[0]
}

func waitForMCPBridgeSessionCount(t *testing.T, p *Plugin, count int) []*session {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		sessions := make([]*session, 0, len(p.sessions))
		for _, sess := range p.sessions {
			sessions = append(sessions, sess)
		}
		p.mu.Unlock()
		if len(sessions) == count {
			return sessions
		}
		if time.Now().After(deadline) {
			t.Fatalf("session count = %d, want %d", len(sessions), count)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForMCPBridgeMapEmpty(t *testing.T, p *Plugin) {
	t.Helper()
	_ = waitForMCPBridgeSessionCount(t, p, 0)
}

func closeMCPSessionForTest(p *Plugin, sess *session) {
	p.closeSession(sess)
}

func assertMCPBridgeNotClosed(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
		t.Fatal(message)
	default:
	}
}

func waitForMCPBridgeClosed(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}
