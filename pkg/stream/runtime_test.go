package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/resource"
	taskruntime "github.com/wklken/apisix-go/pkg/runtime"
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
	router, err := CompileRouter(context.Background(), CompileInput{Revision: 1})
	if err != nil {
		t.Fatalf("CompileRouter() error = %v", err)
	}
	tasks := taskruntime.NewTaskRegistry(ctx, nil)
	owner, err := taskruntime.NewTaskOwner(tasks, "core/stream-runtime", taskruntime.TaskCore)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	runtime := &Runtime{
		ctx:    ctx,
		cancel: cancel,
		source: func() (RouterLease, bool) {
			return RouterLease{Router: router, Release: func() {}}, true
		},
		listeners: listeners,
		tasks:     tasks,
		owner:     owner,
	}
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
	})
	return runtime
}

type routerLeaseFixture struct {
	router *Router

	mu       sync.Mutex
	acquired int
	released int
	started  chan struct{}
	release  chan struct{}
}

func newRouterLeaseFixture(revision byte, block bool, serveErr error) *routerLeaseFixture {
	fixture := &routerLeaseFixture{
		started: make(chan struct{}, 8),
		release: make(chan struct{}, 8),
	}
	fixture.router = &Router{
		routes: []routeEntry{{
			serve: func(ctx context.Context, conn net.Conn, _ string) (string, string, error) {
				fixture.started <- struct{}{}
				if _, err := conn.Write([]byte{revision}); err != nil {
					return "", "tcp", err
				}
				if serveErr != nil {
					return "", "tcp", serveErr
				}
				if block {
					var buffer [1]byte
					readDone := make(chan error, 1)
					go func() {
						_, err := conn.Read(buffer[:])
						readDone <- err
					}()
					select {
					case err := <-readDone:
						return "", "tcp", err
					case <-ctx.Done():
						return "", "tcp", ctx.Err()
					}
				}
				return "", "tcp", nil
			},
		}},
	}
	return fixture
}

func newRuntimeForRoutes(
	t *testing.T,
	ctx context.Context,
	specs []config.TcpListen,
	routes []resource.StreamRoute,
	onResult func(Result),
) (*Runtime, error) {
	t.Helper()
	router, err := compileTestRouter(t, routes, onResult)
	if err != nil {
		return nil, err
	}
	return NewRuntime(ctx, specs, func() (RouterLease, bool) {
		return RouterLease{Router: router, Release: func() {}}, true
	})
}

func (f *routerLeaseFixture) Acquire() (RouterLease, bool) {
	f.mu.Lock()
	f.acquired++
	f.mu.Unlock()
	var once sync.Once
	return RouterLease{
		Router: f.router,
		Release: func() {
			once.Do(func() {
				f.mu.Lock()
				f.released++
				f.mu.Unlock()
				f.release <- struct{}{}
			})
		},
	}, true
}

func (f *routerLeaseFixture) releaseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.released
}

type switchableRouterSource struct {
	mu      sync.RWMutex
	current *routerLeaseFixture
}

func newSwitchableRouterSource(current *routerLeaseFixture) *switchableRouterSource {
	return &switchableRouterSource{current: current}
}

func (s *switchableRouterSource) Acquire() (RouterLease, bool) {
	s.mu.RLock()
	current := s.current
	s.mu.RUnlock()
	if current == nil {
		return RouterLease{}, false
	}
	return current.Acquire()
}

func (s *switchableRouterSource) Store(current *routerLeaseFixture) {
	s.mu.Lock()
	s.current = current
	s.mu.Unlock()
}

func dialRuntimeRevision(t *testing.T, runtime *Runtime) (net.Conn, byte) {
	t.Helper()
	conn, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	var revision [1]byte
	if _, err := io.ReadFull(conn, revision[:]); err != nil {
		_ = conn.Close()
		t.Fatalf("read router revision: %v", err)
	}
	return conn, revision[0]
}

func waitLeaseSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestRuntimePinsRouterLeaseForConnectionLifetime(t *testing.T) {
	old := newRouterLeaseFixture(71, true, nil)
	next := newRouterLeaseFixture(72, false, nil)
	source := newSwitchableRouterSource(old)
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		source.Acquire,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	oldConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 71 {
		t.Fatalf("old connection revision = %d, want 71", revision)
	}
	waitLeaseSignal(t, old.started, "old router did not start")
	source.Store(next)

	nextConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 72 {
		t.Fatalf("new connection revision = %d, want 72", revision)
	}
	_ = nextConn.Close()
	waitLeaseSignal(t, next.release, "new router lease was not released")
	if got := old.releaseCount(); got != 0 {
		t.Fatalf("old release count before connection close = %d, want 0", got)
	}

	_ = oldConn.Close()
	waitLeaseSignal(t, old.release, "old router lease was not released")
}

func TestRuntimeCoreOwnerSurvivesRouterSourceGenerationChange(t *testing.T) {
	first := newRouterLeaseFixture(81, false, nil)
	second := newRouterLeaseFixture(82, false, nil)
	source := newSwitchableRouterSource(first)
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		source.Acquire,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	tasks := runtime.tasks
	firstConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 81 {
		t.Fatalf("first connection revision = %d, want 81", revision)
	}
	_ = firstConn.Close()
	waitLeaseSignal(t, first.release, "first router lease was not released")

	source.Store(second)
	secondConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 82 {
		t.Fatalf("second connection revision = %d, want 82", revision)
	}
	_ = secondConn.Close()
	waitLeaseSignal(t, second.release, "second router lease was not released")

	if runtime.tasks != tasks {
		t.Fatal("router source generation change replaced the runtime task registry")
	}
	if got := runtime.tasks.Active(); !slices.Contains(got, "core/stream-runtime/listener") {
		t.Fatalf("active owners = %v, want core/stream-runtime/listener", got)
	}
	if first.router == second.router {
		t.Fatal("router fixture did not change generation")
	}
}

func TestRuntimeCloseReportsBlockingConnectionResidualAndLaterJoins(t *testing.T) {
	runtime, release, leaseReleased := newBlockingConnectionRuntime(t)
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := runtime.Close(closeCtx)
	var closeErr *runtimeCloseError
	if !errors.As(err, &closeErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() error = %v, want runtime close deadline error", err)
	}
	if got := closeErr.residuals; !reflect.DeepEqual(got, []taskruntime.TaskResidual{{
		Owner: "core/stream-runtime/connection",
	}}) {
		t.Fatalf("residuals = %v, want one connection owner", got)
	}

	release()
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() retry error = %v", err)
	}
	waitLeaseSignal(t, leaseReleased, "blocking connection lease was not released")
}

func newBlockingConnectionRuntime(t *testing.T) (*Runtime, func(), <-chan struct{}) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	leaseReleased := make(chan struct{})
	var releaseTaskOnce sync.Once
	var releaseLeaseOnce sync.Once
	router := &Router{routes: []routeEntry{{
		serve: func(context.Context, net.Conn, string) (string, string, error) {
			close(entered)
			<-release
			return "", "tcp", nil
		},
	}}}
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		func() (RouterLease, bool) {
			return RouterLease{
				Router: router,
				Release: func() {
					releaseLeaseOnce.Do(func() { close(leaseReleased) })
				},
			}, true
		},
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	releaseTask := func() { releaseTaskOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseTask()
		_ = runtime.Close(context.Background())
	})
	conn, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocking connection task did not start")
	}
	return runtime, releaseTask, leaseReleased
}

func TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy(t *testing.T) {
	if os.Getenv("APISIX_GO_TEST_STREAM_CORE_PANIC") == "1" {
		runStreamConnectionCorePanicFixture(t, "stream-core-fatal")
		fmt.Fprintln(os.Stderr, "stream-returned-after-core-panic")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeConnectionTaskPanicUsesCoreFatalPolicy$")
	cmd.Env = append(os.Environ(), "APISIX_GO_TEST_STREAM_CORE_PANIC=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("stream-core-fatal")) ||
		bytes.Contains(output, []byte("stream-returned-after-core-panic")) {
		t.Fatalf("core panic subprocess = %v, output = %s", err, output)
	}
}

func runStreamConnectionCorePanicFixture(t *testing.T, marker string) {
	t.Helper()
	entered := make(chan struct{})
	router := &Router{routes: []routeEntry{{
		serve: func(context.Context, net.Conn, string) (string, string, error) {
			close(entered)
			panic(marker)
		},
	}}}
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		func() (RouterLease, bool) {
			return RouterLease{Router: router, Release: func() {}}, true
		},
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	conn, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("connection panic task did not start")
	}
	time.Sleep(100 * time.Millisecond)
}

func TestRuntimeRejectsConnectionWhenRouterUnavailable(t *testing.T) {
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		func() (RouterLease, bool) { return RouterLease{}, false },
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	conn, err := net.Dial("tcp", runtime.Addresses()[0])
	if err != nil {
		t.Fatalf("dial runtime: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("unavailable router source left connection open")
	}
}

func TestRuntimeReleasesLeaseWhenServeReturnsError(t *testing.T) {
	fixture := newRouterLeaseFixture(73, false, errors.New("serve failure"))
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		fixture.Acquire,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	conn, revision := dialRuntimeRevision(t, runtime)
	if revision != 73 {
		t.Fatalf("connection revision = %d, want 73", revision)
	}
	_ = conn.Close()
	waitLeaseSignal(t, fixture.release, "failed serve did not release router lease")
}

func TestRuntimeTerminalCloseCancelsConnectionsAndReleasesLeases(t *testing.T) {
	fixture := newRouterLeaseFixture(74, true, nil)
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		fixture.Acquire,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	conn, revision := dialRuntimeRevision(t, runtime)
	if revision != 74 {
		t.Fatalf("connection revision = %d, want 74", revision)
	}
	waitLeaseSignal(t, fixture.started, "router did not start")

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitLeaseSignal(t, fixture.release, "terminal close did not release router lease")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("client connection remained open after terminal close")
	}
	_ = conn.Close()
}

func TestRuntimeAcceptFailureDoesNotAcquireLease(t *testing.T) {
	fixture := newRouterLeaseFixture(75, false, nil)
	listener := newScriptedListener(
		"127.0.0.1:21004",
		scriptedAccept{err: errors.New("terminal accept failure")},
	)
	runtime := newTestRuntime(t, listener)
	runtime.source = fixture.Acquire
	startRuntimeListeners(t, runtime, listener)
	waitForAccept(t, listener)
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	fixture.mu.Lock()
	acquired := fixture.acquired
	fixture.mu.Unlock()
	if acquired != 0 {
		t.Fatalf("lease acquisitions after accept failure = %d, want 0", acquired)
	}
}

func TestServeListenerConnectionTaskAdmissionFailureRollsBack(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	listener := newScriptedListener(
		"127.0.0.1:21005",
		scriptedAccept{conn: client},
	)
	ctx, cancel := context.WithCancel(context.Background())
	tasks := taskruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := taskruntime.NewTaskOwner(tasks, "core/stream-runtime", taskruntime.TaskCore)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	if _, err := tasks.Stop(context.Background()); err != nil {
		t.Fatalf("stop task registry: %v", err)
	}
	leaseReleased := make(chan struct{})
	runtime := &Runtime{
		ctx:       ctx,
		cancel:    cancel,
		listeners: []net.Listener{listener},
		conns:     make(map[net.Conn]struct{}),
		tasks:     tasks,
		owner:     owner,
		source: func() (RouterLease, bool) {
			return RouterLease{
				Router: &Router{},
				Release: func() {
					close(leaseReleased)
				},
			}, true
		},
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	runtime.serveListener(ctx, listener)
	select {
	case <-leaseReleased:
	default:
		t.Fatal("connection task admission failure did not release the router lease")
	}
	if runtime.ctx.Err() == nil {
		t.Fatal("connection task admission failure did not initiate runtime close")
	}
	runtime.connMu.Lock()
	tracked := len(runtime.conns)
	runtime.connMu.Unlock()
	if tracked != 0 {
		t.Fatalf("tracked connections after admission failure = %d, want 0", tracked)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection task admission failure left the connection open")
	}
}

func TestNewGenerationRuntimeRequiresRouterSource(t *testing.T) {
	_, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		nil,
	)
	if err == nil {
		t.Fatal("NewRuntime() accepted a nil router source")
	}
}

func TestRuntimeSourceRollbackAffectsOnlyNewConnections(t *testing.T) {
	old := newRouterLeaseFixture(76, true, nil)
	next := newRouterLeaseFixture(77, false, nil)
	source := newSwitchableRouterSource(old)
	runtime, err := NewRuntime(
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		source.Acquire,
	)
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	oldConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 76 {
		t.Fatalf("old connection revision = %d, want 76", revision)
	}
	waitLeaseSignal(t, old.started, "old router did not start")
	source.Store(next)
	nextConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 77 {
		t.Fatalf("next connection revision = %d, want 77", revision)
	}
	_ = nextConn.Close()
	source.Store(old)
	rolledBackConn, revision := dialRuntimeRevision(t, runtime)
	if revision != 76 {
		t.Fatalf("rolled-back connection revision = %d, want 76", revision)
	}
	_ = rolledBackConn.Close()
	waitLeaseSignal(t, old.release, "rolled-back connection lease was not released")
	_ = oldConn.Close()
	waitLeaseSignal(t, old.release, "original pinned lease was not released")
}

func startRuntimeListeners(t *testing.T, runtime *Runtime, listeners ...net.Listener) {
	t.Helper()
	for _, listener := range listeners {
		if err := runtime.owner.Go("listener", func(taskCtx context.Context) error {
			runtime.serveListener(taskCtx, listener)
			return nil
		}); err != nil {
			t.Fatalf("start listener task: %v", err)
		}
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

func TestRuntimeCanceledCloseRejectsSynchronouslyAndLaterCloseJoins(t *testing.T) {
	listener := newScriptedListener("127.0.0.1:21000")
	runtime := newTestRuntime(t, listener)
	release := make(chan struct{})
	if err := runtime.owner.Go("connection", func(context.Context) error {
		<-release
		return nil
	}); err != nil {
		t.Fatalf("start blocked connection task: %v", err)
	}

	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runtime.Close(closeCtx)
	var closeErr *runtimeCloseError
	if !errors.As(err, &closeErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Close(canceled context) error = %v, want %v", err, context.Canceled)
	}
	select {
	case <-listener.closed:
	default:
		t.Fatal("Close(canceled context) returned before rejecting the listener")
	}
	if got := closeErr.residuals; !reflect.DeepEqual(got, []taskruntime.TaskResidual{{
		Owner: "core/stream-runtime/connection",
	}}) {
		t.Fatalf("Close(canceled context) residuals = %v, want connection owner", got)
	}

	close(release)
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatalf("Close() retry error = %v", err)
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
	startRuntimeListeners(t, runtime, listener)

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
	startRuntimeListeners(t, runtime, failing, sibling)

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

func TestRuntimeServesConfiguredListenerAndPublishesResult(t *testing.T) {
	firstUpstream, firstAddr := startStreamUpstream(t, []byte("first-response"))
	defer func() { _ = firstUpstream.Close() }()

	ctx := t.Context()
	results := make(chan Result, 1)
	runtime, err := newRuntimeForRoutes(t,
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "first", firstAddr)},

		func(result Result) { results <- result })
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	firstResponse := runtimeRoundTrip(t, runtime.Addresses()[0], []byte("stream-request"), len("first-response"))
	if string(firstResponse) != "first-response" {
		t.Fatalf("first response = %q, want first-response", firstResponse)
	}
	select {
	case result := <-results:
		if result.Err != nil {
			t.Fatalf("stream result error = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("missing runtime stream result")
	}
}

func TestRuntimeCloseCancelsActiveStream(t *testing.T) {
	upstream, upstreamAddr := startBlockingStreamUpstream(t)
	defer func() { _ = upstream.Close() }()

	runtime, err := newRuntimeForRoutes(t,
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "blocking", upstreamAddr)},

		nil)
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
	runtime, err := newRuntimeForRoutes(t,
		ctx,
		[]config.TcpListen{{Addr: "127.0.0.1:0"}},
		[]resource.StreamRoute{runtimeTestRoute(t, "backpressure", upstreamAddr)},

		nil)
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
	if _, err := newRuntimeForRoutes(t,
		context.Background(),
		[]config.TcpListen{{Addr: "127.0.0.1:0", Tls: true}},
		nil,

		nil); err == nil {
		t.Fatal("NewRuntime() accepted unsupported TLS listener")
	}
	if _, err := newRuntimeForRoutes(t,
		context.Background(),
		[]config.TcpListen{{Addr: "not-an-address"}},
		nil,

		nil); err == nil {
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
	if _, err := newRuntimeForRoutes(t, context.Background(), nil, nil, nil); err == nil {
		t.Fatal("NewRuntime() accepted an empty listener set")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listeners := []config.TcpListen{test.spec}
			if _, err := newRuntimeForRoutes(t, context.Background(), listeners, nil, nil); err == nil {
				t.Fatalf("NewRuntime() accepted unsupported %s", test.name)
			}
		})
	}
}

func TestCompileRouterRejectsUnsupportedAndUnresolvedRoutes(t *testing.T) {
	for _, test := range []struct {
		name  string
		route resource.StreamRoute
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
			if _, err := compileTestRouter(t,
				[]resource.StreamRoute{test.route},

				nil); err == nil {
				t.Fatalf("CompileRouter() accepted %s", test.name)
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

	if _, err := newRuntimeForRoutes(t,
		context.Background(),
		[]config.TcpListen{
			{Addr: firstAddress},
			{Addr: occupied.Addr().String()},
		},
		nil,

		nil); err == nil {
		t.Fatal("NewRuntime() accepted a partially occupied listener set")
	}
	probe, err := net.Listen("tcp", firstAddress)
	if err != nil {
		t.Fatalf("first listener remained bound after rollback: %v", err)
	}
	_ = probe.Close()
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
