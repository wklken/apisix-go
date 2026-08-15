package mcp_bridge

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

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
}

func TestMessageEndpointRejectsOversizedBody(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat", MaxBodySize: 5})
	req := httptest.NewRequest(http.MethodPost, "/message?sessionId=missing", strings.NewReader("123456"))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
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
