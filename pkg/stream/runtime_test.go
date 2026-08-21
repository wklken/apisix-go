package stream

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Temporary() bool { return true }
func (temporaryAcceptError) Timeout() bool   { return true }

type scriptedAccept struct {
	conn net.Conn
	err  error
}

type scriptedListener struct {
	addr      net.Addr
	steps     chan scriptedAccept
	accepted  chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newScriptedListener(addr string, steps ...scriptedAccept) *scriptedListener {
	listener := &scriptedListener{
		addr:     scriptedAddr(addr),
		steps:    make(chan scriptedAccept, len(steps)),
		accepted: make(chan struct{}, len(steps)),
		closed:   make(chan struct{}),
	}
	for _, step := range steps {
		listener.steps <- step
	}
	return listener
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	select {
	case step := <-l.steps:
		l.accepted <- struct{}{}
		return step.conn, step.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *scriptedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedListener) Addr() net.Addr { return l.addr }

type scriptedAddr string

func (a scriptedAddr) Network() string { return "tcp" }
func (a scriptedAddr) String() string  { return string(a) }

func newTestRuntime(t *testing.T, listeners ...net.Listener) *Runtime {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	router, err := NewRouter(nil, nil, nil)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}
	runtime := &Runtime{
		ctx:       ctx,
		cancel:    cancel,
		router:    router,
		listeners: listeners,
		closeDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
	})
	return runtime
}

func startRuntimeListeners(runtime *Runtime, listeners ...net.Listener) {
	runtime.wg.Add(len(listeners))
	for _, listener := range listeners {
		go runtime.serveListener(listener)
	}
}

func waitForAccept(t *testing.T, listener *scriptedListener) {
	t.Helper()
	select {
	case <-listener.accepted:
	case <-time.After(time.Second):
		t.Fatal("listener did not call Accept")
	}
}

func TestServeListenerRetriesTemporaryAcceptErrors(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	listener := newScriptedListener(
		"127.0.0.1:21001",
		scriptedAccept{err: temporaryAcceptError{}},
		scriptedAccept{conn: client},
	)
	runtime := newTestRuntime(t, listener)
	startRuntimeListeners(runtime, listener)

	waitForAccept(t, listener)
	select {
	case <-runtime.ctx.Done():
		t.Fatal("temporary accept failure closed runtime")
	case <-time.After(10 * time.Millisecond):
	}
	waitForAccept(t, listener)
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestServeListenerTerminalErrorClosesRuntimeAndSiblingListeners(t *testing.T) {
	terminalErr := errors.New("terminal accept failure")
	failing := newScriptedListener("127.0.0.1:21002", scriptedAccept{err: terminalErr})
	sibling := newScriptedListener("127.0.0.1:21003")
	runtime := newTestRuntime(t, failing, sibling)
	entries := make(chan logger.Entry, 1)
	stopObserver := logger.ReplaceObserver("stream-runtime-terminal-accept-test", func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stopObserver)
	startRuntimeListeners(runtime, failing, sibling)

	select {
	case <-runtime.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("terminal accept failure did not cancel runtime")
	}
	for name, listener := range map[string]*scriptedListener{
		"failing": failing,
		"sibling": sibling,
	} {
		select {
		case <-listener.closed:
		case <-time.After(time.Second):
			t.Fatalf("%s listener was not closed", name)
		}
	}
	select {
	case entry := <-entries:
		if !strings.Contains(entry.Message, failing.Addr().String()) ||
			!strings.Contains(entry.Message, terminalErr.Error()) {
			t.Fatalf("terminal log = %q, want listener and error", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal accept failure was not logged")
	}
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() after terminal failure = %v", err)
	}
}

func TestRuntimeServesConfiguredListenerAndReloadsRoutes(t *testing.T) {
	firstUpstream, firstAddr := startStreamUpstream(t, []byte("first-response"))
	defer func() { _ = firstUpstream.Close() }()
	secondUpstream, secondAddr := startStreamUpstream(t, []byte("second-response"))
	defer func() { _ = secondUpstream.Close() }()

	ctx := t.Context()
	results := make(chan Result, 2)
	runtime, err := NewRuntime(
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "first", firstAddr)},
		nil,
		func(result Result) { results <- result },
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	firstResponse := runtimeRoundTrip(t, runtime.Addresses()[0], []byte("stream-request"), len("first-response"))
	if string(firstResponse) != "first-response" {
		t.Fatalf("first response = %q, want first-response", firstResponse)
	}
	if err := runtime.Reload([]resource.StreamRoute{runtimeTestRoute(t, "second", secondAddr)}); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}
	secondResponse := runtimeRoundTrip(t, runtime.Addresses()[0], []byte("stream-request"), len("second-response"))
	if string(secondResponse) != "second-response" {
		t.Fatalf("second response = %q, want second-response", secondResponse)
	}

	for range 2 {
		select {
		case result := <-results:
			if result.Err != nil {
				t.Fatalf("stream result error = %v", result.Err)
			}
		case <-time.After(time.Second):
			t.Fatal("missing runtime stream result")
		}
	}
}

func TestRuntimeCloseCancelsActiveStream(t *testing.T) {
	upstream, upstreamAddr := startBlockingStreamUpstream(t)
	defer func() { _ = upstream.Close() }()

	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "blocking", upstreamAddr)},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	client, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("stream-request")); err != nil {
		t.Fatalf("write runtime request: %v", err)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client read succeeded after runtime close")
	}
}

func TestRuntimeCancellationBoundsBackpressure(t *testing.T) {
	upstream, upstreamAddr, accepted, release := startNonReadingStreamUpstream(t)
	defer func() { _ = upstream.Close() }()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	runtime, err := NewRuntime(
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "backpressure", upstreamAddr)},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	client, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	<-accepted

	writeDone := make(chan error, 1)
	go func() {
		payload := make([]byte, 64*1024)
		for range 256 {
			if _, writeErr := client.Write(payload); writeErr != nil {
				writeDone <- writeErr
				return
			}
		}
		writeDone <- nil
	}()
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("large client write completed while upstream was not reading")
		}
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("large client write completed without cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("large client write remained blocked after cancellation")
	}
}

func TestNewRuntimeRejectsTLSAndInvalidAddress(t *testing.T) {
	if _, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0", Tls: true}},
		nil,
		nil,
		nil,
	); err == nil {
		t.Fatal("NewRuntime() accepted unsupported TLS listener")
	}
	if _, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "not-an-address"}},
		nil,
		nil,
		nil,
	); err == nil {
		t.Fatal("NewRuntime() accepted invalid listener address")
	}
}

func TestNewRuntimeRejectsEmptyListenersAndUnsupportedFlags(t *testing.T) {
	tests := []struct {
		name string
		spec config.TcpListen
	}{
		{name: "tls", spec: config.TcpListen{Addr: "127.0.0.1:0", Tls: true}},
		{name: "proxy protocol", spec: config.TcpListen{Addr: "127.0.0.1:0", ProxyProtocol: true}},
		{name: "proxy protocol upstream", spec: config.TcpListen{Addr: "127.0.0.1:0", ProxyProtocolToUpstream: true}},
	}
	if _, err := NewRuntime(context.Background(), nil, nil, nil, nil); err == nil {
		t.Fatal("NewRuntime() accepted an empty listener set")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRuntime(context.Background(), []config.TcpListen{test.spec}, nil, nil, nil); err == nil {
				t.Fatalf("NewRuntime() accepted unsupported %s", test.name)
			}
		})
	}
}

func TestNewRuntimeRejectsUnsupportedAndUnresolvedRoutes(t *testing.T) {
	for _, test := range []struct {
		name  string
		route resource.StreamRoute
		flags []string
	}{
		{
			name: "unresolved upstream",
			route: resource.StreamRoute{
				ID:         "unresolved",
				UpstreamID: "missing",
			},
		},
		{
			name: "unsupported plugin",
			route: resource.StreamRoute{
				ID: "unsupported-plugin",
				Plugins: map[string]resource.PluginConfig{
					"unsupported": nil,
				},
				Upstream: resource.Upstream{
					Scheme: "tcp",
					Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
				},
			},
		},
		{
			name: "TLS upstream",
			route: resource.StreamRoute{
				ID: "tls-upstream",
				Upstream: resource.Upstream{
					Scheme: "tcp",
					TLS:    &resource.UpstreamTLS{},
					Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRuntime(
				context.Background(),
				[]config.TcpListen{{Addr: "127.0.0.1:0"}},
				[]resource.StreamRoute{test.route},
				test.flags,
				nil,
			); err == nil {
				t.Fatalf("NewRuntime() accepted %s", test.name)
			}
		})
	}
}

func TestNewRuntimeRollsBackEarlierListenerOnBindFailure(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve first listener address: %v", err)
	}
	firstAddress := first.Addr().String()
	_ = first.Close()

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy listener: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	if _, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{
			{Addr: firstAddress},
			{Addr: occupied.Addr().String()},
		},
		nil,
		nil,
		nil,
	); err == nil {
		t.Fatal("NewRuntime() accepted a partially occupied listener set")
	}
	probe, err := net.Listen("tcp", firstAddress)
	if err != nil {
		t.Fatalf("first listener remained bound after rollback: %v", err)
	}
	_ = probe.Close()
}

func TestRuntimeReloadRejectsInvalidRoutesAndKeepsLastGood(t *testing.T) {
	upstream, upstreamAddr := startStreamUpstream(t, []byte("last-good"))
	defer func() { _ = upstream.Close() }()
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "last-good", upstreamAddr)},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	if err := runtime.Reload([]resource.StreamRoute{{ID: "invalid", UpstreamID: "missing"}}); err == nil {
		t.Fatal("Reload() accepted an unresolved route")
	}
	if err := runtime.Reload([]resource.StreamRoute{{
		ID: "invalid-tls",
		Upstream: resource.Upstream{
			Scheme: "tcp",
			TLS:    &resource.UpstreamTLS{},
			Nodes:  []resource.Node{{Host: "127.0.0.1", Port: 1, Weight: 1}},
		},
	}}); err == nil {
		t.Fatal("Reload() accepted a TLS stream upstream")
	}
	got := runtimeRoundTrip(
		t,
		runtime.Addresses()[0],
		[]byte("stream-request"),
		len("last-good"),
	)
	if string(got) != "last-good" {
		t.Fatalf("last-good response = %q, want last-good", got)
	}
}

func runtimeTestRoute(t *testing.T, id, upstreamAddr string) resource.StreamRoute {
	t.Helper()
	host, portText, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		t.Fatalf("split upstream address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	return resource.StreamRoute{
		ID: id,
		Upstream: resource.Upstream{
			Scheme: "tcp",
			Nodes:  []resource.Node{{Host: host, Port: port, Weight: 1}},
		},
	}
}

func runtimeRoundTrip(t *testing.T, address string, request []byte, responseSize int) []byte {
	t.Helper()
	client, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	if _, err := client.Write(request); err != nil {
		_ = client.Close()
		t.Fatalf("write runtime request: %v", err)
	}
	response := make([]byte, responseSize)
	if _, err := io.ReadFull(client, response); err != nil {
		_ = client.Close()
		t.Fatalf("read runtime response: %v", err)
	}
	_ = client.Close()
	return response
}

func startBlockingStreamUpstream(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen blocking upstream: %v", err)
	}
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(io.Discard, conn)
	}()
	return listener, listener.Addr().String()
}

func startNonReadingStreamUpstream(t *testing.T) (net.Listener, string, <-chan struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen non-reading upstream: %v", err)
	}
	accepted := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		close(accepted)
		<-release
	}()
	return listener, listener.Addr().String(), accepted, func() { close(release) }
}
