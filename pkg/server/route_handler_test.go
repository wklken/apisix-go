package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
	"github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type panicSnapshotLogPlugin struct {
	base.BasePlugin
	snapshots []base.LogSnapshot
	panicLog  bool
}

func (p *panicSnapshotLogPlugin) Init() error                            { return nil }
func (p *panicSnapshotLogPlugin) PostInit() error                        { return nil }
func (p *panicSnapshotLogPlugin) Config() any                            { return nil }
func (p *panicSnapshotLogPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *panicSnapshotLogPlugin) RunLogPhase(snapshot base.LogSnapshot) error {
	p.snapshots = append(p.snapshots, snapshot)
	if p.panicLog {
		panic("detached logger panic")
	}
	return nil
}

type rejectingIngressAuthPlugin struct {
	base.BasePlugin
}

func (p *rejectingIngressAuthPlugin) Init() error                            { return nil }
func (p *rejectingIngressAuthPlugin) PostInit() error                        { return nil }
func (p *rejectingIngressAuthPlugin) Config() any                            { return nil }
func (p *rejectingIngressAuthPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *rejectingIngressAuthPlugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	w.WriteHeader(http.StatusUnauthorized)
	return base.StopRequest(r)
}

func TestServeRouteRequestOwnsConsumerIdentityAtIngress(t *testing.T) {
	tests := []struct {
		name       string
		withAuth   bool
		wantStatus int
	}{
		{name: "unauthenticated", wantStatus: http.StatusNoContent},
		{name: "failed authentication", withAuth: true, wantStatus: http.StatusUnauthorized},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loggerPlugin := &panicSnapshotLogPlugin{}
			loggerPlugin.Name = "test-ingress-logger"
			bindings := []pluginpkg.Binding{
				pluginpkg.BindPlugin(
					"http-logger",
					loggerPlugin,
					pluginpkg.ScopeRoute,
					pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: test.name},
				),
			}
			if test.withAuth {
				authPlugin := &rejectingIngressAuthPlugin{}
				authPlugin.Name = "jwt-auth"
				bindings = append(bindings, pluginpkg.BindPlugin(
					"jwt-auth",
					authPlugin,
					pluginpkg.ScopeRoute,
					pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: test.name},
				))
			}
			executor, err := pluginpkg.NewLogExecutorFromBindings(bindings)
			if err != nil {
				t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
			}
			handler := pluginpkg.NewRequestPipeline(bindings, nil).
				WithLogExecutor(&executor).
				Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got := r.Header.Get("X-Consumer-Username"); got != "" {
						t.Errorf("upstream consumer header = %q, want unset", got)
					}
					w.WriteHeader(http.StatusNoContent)
				}))

			request := httptest.NewRequest(http.MethodGet, "http://gateway.test/identity", nil)
			request.Header.Set("X-Consumer-Username", "attacker")
			response := httptest.NewRecorder()
			serveRouteRequest(response, request, handler)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if got := request.Header.Get("X-Consumer-Username"); got != "" {
				t.Fatalf("ingress request consumer header = %q, want unset", got)
			}
			if len(loggerPlugin.snapshots) != 1 {
				t.Fatalf("detached log snapshots = %d, want 1", len(loggerPlugin.snapshots))
			}
			if got := loggerPlugin.snapshots[0].Request.Header.Get("X-Consumer-Username"); got != "" {
				t.Fatalf("detached log consumer header = %q, want unset", got)
			}
		})
	}
}

type failingRouteResponseWriter struct {
	header      http.Header
	writeStatus int
	writeN      int
	writeErr    error
	panicWrite  bool
}

func (w *failingRouteResponseWriter) Header() http.Header { return w.header }

func (w *failingRouteResponseWriter) WriteHeader(status int) { w.writeStatus = status }

func (w *failingRouteResponseWriter) Write([]byte) (int, error) {
	if w.panicWrite {
		panic("response write failed")
	}
	return w.writeN, w.writeErr
}

type flushingRouteResponseWriter struct {
	header      http.Header
	writeStatus int
	flushes     int
}

func (w *flushingRouteResponseWriter) Header() http.Header { return w.header }

func (w *flushingRouteResponseWriter) WriteHeader(status int) { w.writeStatus = status }

func (w *flushingRouteResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *flushingRouteResponseWriter) Flush() { w.flushes++ }

type hijackingRouteResponseWriter struct {
	header      http.Header
	writeStatus int
	conn        net.Conn
}

type failingHijackRouteResponseWriter struct {
	header http.Header
	err    error
}

func (w *failingHijackRouteResponseWriter) Header() http.Header { return w.header }

func (w *failingHijackRouteResponseWriter) WriteHeader(int) {}

func (w *failingHijackRouteResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *failingHijackRouteResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, w.err
}

type blockingHeaderRouteResponseWriter struct {
	header  http.Header
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (w *blockingHeaderRouteResponseWriter) Header() http.Header { return w.header }

func (w *blockingHeaderRouteResponseWriter) WriteHeader(int) {
	w.once.Do(func() { close(w.started) })
	<-w.unblock
}

func (w *blockingHeaderRouteResponseWriter) Write(body []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return len(body), nil
}

func (w *hijackingRouteResponseWriter) Header() http.Header { return w.header }

func (w *hijackingRouteResponseWriter) WriteHeader(status int) { w.writeStatus = status }

func (w *hijackingRouteResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *hijackingRouteResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

type countedHTTPLeaseFixture struct {
	owner    *generationOwner
	acquires atomic.Int64
	releases atomic.Int64
}

func newCountedHTTPLeaseFixture(t *testing.T, revision uint64) *countedHTTPLeaseFixture {
	t.Helper()
	fixture := newTestGenerationOwner(t, revision, true, false)
	fixture.owner.activateDomains(ownerDomainHTTP)
	return &countedHTTPLeaseFixture{owner: fixture.owner}
}

func (fixture *countedHTTPLeaseFixture) Acquire() (httpGenerationLease, bool) {
	lease, ok := fixture.owner.acquireHTTP()
	if !ok {
		return httpGenerationLease{}, false
	}
	fixture.acquires.Add(1)
	return fixture.count(lease), true
}

func (fixture *countedHTTPLeaseFixture) count(lease httpGenerationLease) httpGenerationLease {
	baseRelease := lease.Release
	baseRetain := lease.retain
	var releaseOnce sync.Once
	lease.Release = func() {
		releaseOnce.Do(func() {
			fixture.releases.Add(1)
			baseRelease()
		})
	}
	lease.retain = func() (httpGenerationLease, bool) {
		child, ok := baseRetain()
		if !ok {
			return httpGenerationLease{}, false
		}
		fixture.acquires.Add(1)
		return fixture.count(child), true
	}
	return lease
}

type switchableHTTPLeaseSource struct {
	current atomic.Pointer[countedHTTPLeaseFixture]
}

func newSwitchableHTTPLeaseSource(current *countedHTTPLeaseFixture) *switchableHTTPLeaseSource {
	source := &switchableHTTPLeaseSource{}
	source.Store(current)
	return source
}

func (source *switchableHTTPLeaseSource) Store(current *countedHTTPLeaseFixture) {
	source.current.Store(current)
}

func (source *switchableHTTPLeaseSource) Acquire() (httpGenerationLease, bool) {
	current := source.current.Load()
	if current == nil {
		return httpGenerationLease{}, false
	}
	return current.Acquire()
}

func TestRouteHandlerRequestRetainsLoadedGenerationAcrossSwap(t *testing.T) {
	old := newCountedHTTPLeaseFixture(t, 301)
	next := newCountedHTTPLeaseFixture(t, 302)
	source := newSwitchableHTTPLeaseSource(old)
	routes := newGenerationRouteHandler(source.Acquire)
	started := make(chan struct{})
	unblock := make(chan struct{})
	writer := &blockingHeaderRouteResponseWriter{
		header: make(http.Header), started: started, unblock: unblock,
	}
	done := make(chan struct{})
	go func() {
		routes.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/old", nil))
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request did not reach its loaded generation")
	}

	source.Store(next)
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/next", nil))
	if got := old.releases.Load(); got != 0 {
		t.Fatalf("old release count = %d before request exit, want 0", got)
	}
	if got := next.releases.Load(); got != 1 {
		t.Fatalf("next release count = %d, want 1", got)
	}
	close(unblock)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old request did not finish")
	}
	if got := old.releases.Load(); got != 1 {
		t.Fatalf("old release count = %d, want 1", got)
	}
}

func TestRouteHandlerUnavailableHTTPDomainReturns503(t *testing.T) {
	routes := newGenerationRouteHandler(func() (httpGenerationLease, bool) {
		return httpGenerationLease{}, false
	})
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestRouteHandlerNilGenerationSourceAndReleaseFailClosed(t *testing.T) {
	t.Run("nil source", func(t *testing.T) {
		routes := newGenerationRouteHandler(nil)
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		routes.Close()
		routes.Close()
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("nil release", func(t *testing.T) {
		fixture := newCountedHTTPLeaseFixture(t, 311)
		lease, ok := fixture.Acquire()
		if !ok {
			t.Fatal("fixture lease unavailable")
		}
		release := lease.Release
		lease.Release = nil
		t.Cleanup(release)
		routes := newGenerationRouteHandler(func() (httpGenerationLease, bool) { return lease, true })
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
		}
	})
}

func TestRouteHandlerBatchDispatchRetainsParentGenerationAfterSwap(t *testing.T) {
	old := newCountedHTTPLeaseFixture(t, 303)
	next := newCountedHTTPLeaseFixture(t, 304)
	source := newSwitchableHTTPLeaseSource(old)
	parent, ok := source.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	source.Store(next)
	dispatched := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		factory := batch_requests.DispatchLeaseFactoryFromRequest(request)
		if factory == nil {
			t.Fatal("dispatch lease factory is nil")
		}
		child, acquired := factory()
		if !acquired {
			t.Fatal("dispatch lease was not retained")
		}
		child.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/child", nil))
		child.Release()
		child.Release()
		dispatched = true
		w.WriteHeader(http.StatusNoContent)
	})
	serveRouteRequestForHTTPGeneration(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/batch", nil), handler, &parent, nil,
	)
	parent.Release()
	if !dispatched {
		t.Fatal("child dispatch did not run")
	}
	if got := old.acquires.Load(); got != 2 {
		t.Fatalf("old acquire count = %d, want parent plus child", got)
	}
	if got := old.releases.Load(); got != 2 {
		t.Fatalf("old release count = %d, want parent plus child", got)
	}
	if got := next.acquires.Load(); got != 0 {
		t.Fatalf("next acquire count = %d, child reacquired current generation", got)
	}
}

func TestGenerationHijackNaturalCloseReleasesLease(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 305)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	left, right := net.Pipe()
	closed := &atomic.Int32{}
	raw := &countingCloseConn{Conn: left, closed: closed}
	t.Cleanup(func() {
		_ = right.Close()
		_ = raw.Close()
	})
	var connection net.Conn
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var err error
		connection, _, err = w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&hijackingRouteResponseWriter{header: make(http.Header), conn: raw},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	if got := fixture.releases.Load(); got != 1 {
		t.Fatalf("release count after request = %d, want 1", got)
	}
	fixture.owner.deactivateDomains(ownerDomainHTTP)
	if got := closed.Load(); got != 0 {
		t.Fatalf("ordinary retirement closed hijack %d times", got)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("generation connection Close() error = %v", err)
	}
	if got := fixture.releases.Load(); got != 2 {
		t.Fatalf("release count after natural close = %d, want 2", got)
	}
}

func TestRouteHandlerTerminalCloseForcesHijackAndReleasesLease(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 306)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	left, right := net.Pipe()
	closed := &atomic.Int32{}
	raw := &countingCloseConn{Conn: left, closed: closed}
	t.Cleanup(func() { _ = right.Close() })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&hijackingRouteResponseWriter{header: make(http.Header), conn: raw},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	routes.Close()
	if got := closed.Load(); got != 1 {
		t.Fatalf("terminal close count = %d, want 1", got)
	}
	if got := fixture.releases.Load(); got != 2 {
		t.Fatalf("release count after terminal close = %d, want 2", got)
	}
}

func TestRouteHandlerTerminalCloseAndNaturalHijackCloseAreExactlyOnce(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 312)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	left, right := net.Pipe()
	closed := &atomic.Int32{}
	raw := &countingCloseConn{Conn: left, closed: closed}
	t.Cleanup(func() { _ = right.Close() })
	var connection net.Conn
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var err error
		connection, _, err = w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&hijackingRouteResponseWriter{header: make(http.Header), conn: raw},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		routes.Close()
	}()
	go func() {
		defer wait.Done()
		<-start
		_ = connection.Close()
	}()
	close(start)
	wait.Wait()
	if got := closed.Load(); got != 1 {
		t.Fatalf("underlying close count = %d, want 1", got)
	}
	if got := fixture.releases.Load(); got != 2 {
		t.Fatalf("release count = %d, want request plus hijack", got)
	}
}

func TestGenerationHijackFailureDoesNotRetainLease(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 307)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	wantErr := errors.New("hijack failed")
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); !errors.Is(err, wantErr) {
			t.Fatalf("Hijack() error = %v, want %v", err, wantErr)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&failingHijackRouteResponseWriter{header: make(http.Header), err: wantErr},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	if got := fixture.acquires.Load(); got != 1 {
		t.Fatalf("acquire count = %d after failed hijack, want 1", got)
	}
	if got := fixture.releases.Load(); got != 1 {
		t.Fatalf("release count = %d after failed hijack, want 1", got)
	}
}

func TestGenerationHijackRetainFailureClosesRawAndReturnsError(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 309)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	parent.Release()
	left, right := net.Pipe()
	closed := &atomic.Int32{}
	raw := &countingCloseConn{Conn: left, closed: closed}
	t.Cleanup(func() { _ = right.Close() })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, readWriter, err := w.(http.Hijacker).Hijack()
		if !errors.Is(err, errHTTPGenerationHijackUnavailable) {
			t.Fatalf("Hijack() error = %v, want %v", err, errHTTPGenerationHijackUnavailable)
		}
		if connection != nil || readWriter != nil {
			t.Fatalf("failed hijack returned connection/read-writer = %v/%v", connection, readWriter)
		}
	})
	serveRouteRequestForHTTPGeneration(
		&hijackingRouteResponseWriter{header: make(http.Header), conn: raw},
		httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	if got := closed.Load(); got != 1 {
		t.Fatalf("raw close count = %d, want 1", got)
	}
	if got := fixture.acquires.Load(); got != 1 {
		t.Fatalf("acquire count = %d, retain failure created a child", got)
	}
}

func TestGenerationHijackRebuildsReadWriterAroundWrappedConnection(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 310)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	original := bufio.NewReadWriter(bufio.NewReader(left), bufio.NewWriter(left))
	writeDone := make(chan error, 1)
	go func() {
		_, err := right.Write([]byte("prefetched"))
		writeDone <- err
	}()
	originalReader := original.Reader
	if _, err := originalReader.Peek(len("prefetched")); err != nil {
		t.Fatalf("prefill raw hijack reader: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("prefill raw hijack writer: %v", err)
	}
	writer := &fixedReadWriterHijackResponseWriter{
		header: make(http.Header), conn: left, readWriter: original,
	}
	var returned net.Conn
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var readWriter *bufio.ReadWriter
		var err error
		returned, readWriter, err = w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := returned.(*generationConn); !ok {
			t.Fatalf("returned connection type = %T, want *generationConn", returned)
		}
		if readWriter == original {
			t.Fatal("hijack returned raw connection read-writer")
		}
		buffer := make([]byte, len("prefetched"))
		if _, err := io.ReadFull(readWriter, buffer); err != nil {
			t.Fatalf("read preserved hijack buffer: %v", err)
		}
		if string(buffer) != "prefetched" {
			t.Fatalf("preserved hijack bytes = %q, want prefetched", buffer)
		}
	})
	serveRouteRequestForHTTPGeneration(
		writer, httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
	)
	parent.Release()
	if err := returned.Close(); err != nil {
		t.Fatal(err)
	}
}

type fixedReadWriterHijackResponseWriter struct {
	header     http.Header
	conn       net.Conn
	readWriter *bufio.ReadWriter
}

func (w *fixedReadWriterHijackResponseWriter) Header() http.Header { return w.header }

func (w *fixedReadWriterHijackResponseWriter) WriteHeader(int) {}

func (w *fixedReadWriterHijackResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *fixedReadWriterHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.readWriter, nil
}

func TestGenerationHijackPanicClosesConnectionAndReleasesLease(t *testing.T) {
	fixture := newCountedHTTPLeaseFixture(t, 308)
	routes := newGenerationRouteHandler(fixture.Acquire)
	parent, ok := fixture.Acquire()
	if !ok {
		t.Fatal("parent lease unavailable")
	}
	left, right := net.Pipe()
	closed := &atomic.Int32{}
	raw := &countingCloseConn{Conn: left, closed: closed}
	t.Cleanup(func() { _ = right.Close() })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		panic("after generation hijack")
	})
	mustAbortHandlerPanic(t, func() {
		serveRouteRequestForHTTPGeneration(
			&hijackingRouteResponseWriter{header: make(http.Header), conn: raw},
			httptest.NewRequest(http.MethodGet, "/hijack", nil), handler, &parent, routes,
		)
	})
	parent.Release()
	if got := closed.Load(); got != 1 {
		t.Fatalf("panic close count = %d, want 1", got)
	}
	if got := fixture.releases.Load(); got != 2 {
		t.Fatalf("panic release count = %d, want 2", got)
	}
}

func mustAbortHandlerPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("panic = %#v, want %v", got, http.ErrAbortHandler)
		}
	}()
	fn()
}

func TestRouteHandlerPanicBeforeCommitReturnsStableJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Leaked", "secret")
		panic("application panic")
	})

	serveRouteRequest(recorder, request, handler)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if got := recorder.Body.String(); got != `{"message":"Internal Server Error"}` {
		t.Fatalf("body = %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("content type = %q", got)
	}
	if recorder.Header().Get("X-Leaked") != "" {
		t.Fatal("panic response leaked pre-commit headers")
	}
}

func TestPluginPhaseClosurePreTerminalPanicLogsAndRecycles(t *testing.T) {
	loggerPlugin := &panicSnapshotLogPlugin{}
	loggerPlugin.Name = "test-logger"
	loggerBinding := pluginpkg.BindPlugin(
		"http-logger",
		loggerPlugin,
		pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "panic-log"},
	)
	executor, err := pluginpkg.NewLogExecutorFromBindings([]pluginpkg.Binding{loggerBinding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	var derived *http.Request
	pipeline := pluginpkg.NewRequestPipeline(
		[]pluginpkg.Binding{loggerBinding},
		nil,
	).WithLogExecutor(&executor)
	handler := pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		derived = r
		apisixctx.RegisterApisixVar(r, "$panic_marker", "visible-to-finalizer")
		panic("before terminal response")
	}))
	recorder := httptest.NewRecorder()

	serveRouteRequest(recorder, httptest.NewRequest(http.MethodGet, "/panic-log", nil), handler)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if len(loggerPlugin.snapshots) != 1 {
		t.Fatalf("log snapshots = %d, want 1", len(loggerPlugin.snapshots))
	}
	snapshot := loggerPlugin.snapshots[0]
	if snapshot.Outcome.Kind != apisixctx.RequestOutcomeRecoveredPanic ||
		snapshot.Outcome.Status != http.StatusInternalServerError ||
		snapshot.Source != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("panic snapshot outcome/source = %#v/%q", snapshot.Outcome, snapshot.Source)
	}
	if got := snapshot.Request.APISIXVars["$panic_marker"]; got != "visible-to-finalizer" {
		t.Fatalf("panic snapshot marker = %#v", got)
	}
	if got := apisixctx.GetApisixVar(derived, "$panic_marker"); got != "" {
		t.Fatalf("panic marker after recycle = %#v, want empty", got)
	}
}

func TestPluginPhaseClosureLoggerPanicStillFinalizesAndRecycles(t *testing.T) {
	loggerPlugin := &panicSnapshotLogPlugin{panicLog: true}
	loggerPlugin.Name = "test-logger"
	loggerBinding := pluginpkg.BindPlugin(
		"http-logger",
		loggerPlugin,
		pluginpkg.ScopeRoute,
		pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "logger-panic"},
	)
	executor, err := pluginpkg.NewLogExecutorFromBindings([]pluginpkg.Binding{loggerBinding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	var derived *http.Request
	handler := pluginpkg.NewRequestPipeline([]pluginpkg.Binding{loggerBinding}, nil).
		WithLogExecutor(&executor).
		Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			derived = r
			apisixctx.RegisterApisixVar(r, "$logger_panic_marker", "live")
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
			w.WriteHeader(http.StatusNoContent)
		}))
	recorder := httptest.NewRecorder()

	serveRouteRequest(recorder, httptest.NewRequest(http.MethodGet, "/logger-panic", nil), handler)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	if len(loggerPlugin.snapshots) != 1 {
		t.Fatalf("logger calls = %d, want 1", len(loggerPlugin.snapshots))
	}
	if got := loggerPlugin.snapshots[0].Request.APISIXVars["$logger_panic_marker"]; got != "live" {
		t.Fatalf("logger snapshot marker = %#v, want live", got)
	}
	if got := apisixctx.GetApisixVar(derived, "$logger_panic_marker"); got != "" {
		t.Fatalf("marker after recycle = %#v, want empty", got)
	}
}

func TestRouteHandlerCompletesLifecycleAndAttachesCaptureBeforeFinalizers(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/complete", nil)
	var finishedAt time.Time
	var outcome apisixctx.ResponseOutcome
	var capturedBody string
	var markerDuringFinalize any
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture, ok := base.ResponseCaptureFromRequest(r)
		if !ok {
			t.Fatal("response capture is not attached to route request")
		}
		if err := capture.EnableBodyCapture(32); err != nil {
			t.Fatalf("EnableBodyCapture() error = %v", err)
		}
		apisixctx.RegisterApisixVar(r, "$test_marker", "live")
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if lifecycle == nil || !lifecycle.AddFinalizer("observe", func() error {
			finishedAt = lifecycle.FinishedAt()
			outcome = lifecycle.Outcome()
			capturedBody = string(capture.Snapshot().Body)
			markerDuringFinalize = apisixctx.GetApisixVar(r, "$test_marker")
			return nil
		}) {
			t.Fatal("failed to register observer finalizer")
		}
		_, _ = w.Write([]byte("complete"))
	})

	serveRouteRequest(recorder, request, handler)
	if finishedAt.IsZero() {
		t.Fatal("FinishedAt() is zero during finalization")
	}
	if outcome.Kind != apisixctx.RequestOutcomeCompleted || outcome.Status != http.StatusOK ||
		outcome.Bytes != int64(len("complete")) {
		t.Fatalf("finalizer outcome = %#v", outcome)
	}
	if capturedBody != "complete" {
		t.Fatalf("captured body = %q, want complete", capturedBody)
	}
	if markerDuringFinalize != "live" {
		t.Fatalf("marker during finalization = %#v, want live", markerDuringFinalize)
	}
}

func TestRouteHandlerBodyLimitFinalizesCanonicalOutcome(t *testing.T) {
	var finalOutcome apisixctx.ResponseOutcome
	var finalSource apisixctx.ResponseSource
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if lifecycle == nil || !lifecycle.AddFinalizer("observe-body-limit", func() error {
			finalOutcome = lifecycle.Outcome()
			finalSource = lifecycle.ResponseSource()
			return nil
		}) {
			t.Fatal("failed to register body-limit finalizer")
		}
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Stale", "remove-me")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream failure"))
	})
	routes := newRouteHandler(handler, nil)
	limitedRoutes := limitRequestBody(routes, 3)
	request := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("abcd"))
	request.ContentLength = -1
	response := httptest.NewRecorder()

	limitedRoutes.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if got, want := response.Body.String(), `{"message":"request body too large"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if response.Header().Get("X-Stale") != "" {
		t.Fatal("suppressed downstream header survived canonical 413")
	}
	if finalOutcome.Status != http.StatusRequestEntityTooLarge || !finalOutcome.Committed ||
		finalOutcome.Bytes != int64(len(`{"message":"request body too large"}`)) {
		t.Fatalf("finalizer outcome = %#v, want committed canonical 413", finalOutcome)
	}
	if finalSource != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("finalizer response source = %q, want %q", finalSource, apisixctx.ResponseSourceAPISIX)
	}
}

func TestRouteHandlerBodyLimitFinalizesCanonicalOutcomeBeforePanicRecovery(t *testing.T) {
	var finalOutcome apisixctx.ResponseOutcome
	var finalSource apisixctx.ResponseSource
	routes := newRouteHandler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if lifecycle == nil || !lifecycle.AddFinalizer("observe-body-limit-panic", func() error {
			finalOutcome = lifecycle.Outcome()
			finalSource = lifecycle.ResponseSource()
			return nil
		}) {
			t.Fatal("failed to register body-limit panic finalizer")
		}
		_, _ = io.ReadAll(r.Body)
		panic("after request body overflow")
	}), nil)
	handler := limitRequestBody(routes, 3)
	request := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("abcd"))
	request.ContentLength = -1
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertRequestBodyLimitResponse(t, response)
	if finalOutcome.Kind != apisixctx.RequestOutcomeRecoveredPanic {
		t.Fatalf("finalizer outcome kind = %q, want recovered panic", finalOutcome.Kind)
	}
	if finalOutcome.Status != http.StatusRequestEntityTooLarge || !finalOutcome.Committed ||
		finalOutcome.Bytes != int64(len(requestBodyLimitTestMessage)) {
		t.Fatalf("finalizer outcome = %#v, want committed canonical 413", finalOutcome)
	}
	if finalSource != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("finalizer response source = %q, want %q", finalSource, apisixctx.ResponseSourceAPISIX)
	}
}

func TestRouteHandlerPanicResponseWriteFailureStillFinalizesAndAborts(t *testing.T) {
	tests := []struct {
		name      string
		writer    *failingRouteResponseWriter
		wantBytes int64
	}{
		{
			name:   "write-error",
			writer: &failingRouteResponseWriter{header: make(http.Header), writeErr: errors.New("write failed")},
		},
		{
			name:      "short-write",
			writer:    &failingRouteResponseWriter{header: make(http.Header), writeN: 1},
			wantBytes: 1,
		},
		{
			name:   "write-panic",
			writer: &failingRouteResponseWriter{header: make(http.Header), panicWrite: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/panic", nil)
			var derived *http.Request
			var finalOutcome apisixctx.ResponseOutcome
			var calls []string
			var markerDuringFinalize any
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				derived = r
				apisixctx.RegisterApisixVar(r, "$test_marker", "live")
				lifecycle := apisixctx.GetRequestLifecycle(r)
				if lifecycle == nil || !lifecycle.AddFinalizer("first", func() error {
					calls = append(calls, "first")
					return nil
				}) || !lifecycle.AddFinalizer("observe", func() error {
					calls = append(calls, "observe")
					finalOutcome = lifecycle.Outcome()
					markerDuringFinalize = apisixctx.GetApisixVar(r, "$test_marker")
					return nil
				}) {
					t.Fatal("failed to register finalizers")
				}
				panic("application panic")
			})

			mustAbortHandlerPanic(t, func() { serveRouteRequest(test.writer, request, handler) })
			if got, want := strings.Join(calls, ","), "observe,first"; got != want {
				t.Fatalf("finalizer calls = %q, want %q", got, want)
			}
			if finalOutcome.Kind != apisixctx.RequestOutcomeAbortedPanic ||
				finalOutcome.Status != http.StatusInternalServerError ||
				!finalOutcome.Committed || finalOutcome.Bytes != test.wantBytes {
				t.Fatalf("finalizer outcome = %#v", finalOutcome)
			}
			if markerDuringFinalize != "live" {
				t.Fatalf("marker during finalization = %#v, want live", markerDuringFinalize)
			}
			if got := apisixctx.GetApisixVar(derived, "$test_marker"); got != "" {
				t.Fatalf("marker after recycling = %#v, want empty", got)
			}
			if test.writer.writeStatus != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 attempt", test.writer.writeStatus)
			}
		})
	}
}

func TestRouteHandlerPanicAfterWriteAbortsWithoutSecondResponse(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
		panic("after write")
	})

	mustAbortHandlerPanic(t, func() {
		serveRouteRequest(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil), handler)
	})
	if got := recorder.Body.String(); got != "first" {
		t.Fatalf("body = %q, want first response only", got)
	}
}

func TestRouteHandlerPanicAfterFlushAbortsWithoutSecondResponse(t *testing.T) {
	writer := &flushingRouteResponseWriter{header: make(http.Header)}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush()
		panic("after flush")
	})

	mustAbortHandlerPanic(t, func() {
		serveRouteRequest(writer, httptest.NewRequest(http.MethodGet, "/panic", nil), handler)
	})
	if writer.flushes != 1 {
		t.Fatalf("flushes = %d, want 1", writer.flushes)
	}
}

func TestRouteHandlerPanicAfterHijackAbortsWithoutSecondResponse(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	closed := &atomic.Int32{}
	writer := &hijackingRouteResponseWriter{
		header: make(http.Header),
		conn:   &countingCloseConn{Conn: left, closed: closed},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
		panic("after hijack")
	})

	mustAbortHandlerPanic(t, func() {
		serveRouteRequest(writer, httptest.NewRequest(http.MethodGet, "/panic", nil), handler)
	})
	if got := closed.Load(); got != 1 {
		t.Fatalf("hijacked close count = %d, want 1", got)
	}
}

func TestRouteHandlerSuccessfulHijackRetainsConnection(t *testing.T) {
	left, right := net.Pipe()
	counting := &countingCloseConn{Conn: left, closed: &atomic.Int32{}}
	t.Cleanup(func() {
		_ = right.Close()
		_ = left.Close()
	})
	writer := &hijackingRouteResponseWriter{header: make(http.Header), conn: counting}
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
			t.Fatalf("Hijack() error = %v", err)
		}
	})

	serveRouteRequest(writer, httptest.NewRequest(http.MethodGet, "/hijack", nil), handler)
	if got := counting.closed.Load(); got != 0 {
		t.Fatalf("normal hijack close count = %d, want 0", got)
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := right.Write([]byte("ok"))
		writeDone <- err
	}()
	buffer := make([]byte, 2)
	if _, err := io.ReadFull(counting, buffer); err != nil {
		t.Fatalf("retained connection read error = %v", err)
	}
	if string(buffer) != "ok" {
		t.Fatalf("retained connection bytes = %q, want ok", buffer)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("retained connection write error = %v", err)
	}
}

func TestRouteHandlerRetiresHijackedConnectionBeforeGenerationDrain(t *testing.T) {
	tests := []struct {
		name  string
		close func(*routeHandler)
	}{
		{name: "replace", close: func(routes *routeHandler) {
			routes.Replace(http.NotFoundHandler(), nil)
		}},
		{name: "close", close: func(routes *routeHandler) { routes.Close() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, right := net.Pipe()
			t.Cleanup(func() {
				_ = left.Close()
				_ = right.Close()
			})
			closed := &atomic.Int32{}
			connection := &countingCloseConn{Conn: left, closed: closed}
			started := make(chan struct{})
			requestDone := make(chan struct{})
			stopped := make(chan struct{})
			routes := newRouteHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
					t.Errorf("Hijack() error = %v", err)
					return
				}
				close(started)
				_, _ = connection.Read(make([]byte, 1))
				close(requestDone)
			}), func() { close(stopped) })

			go func() {
				routes.ServeHTTP(&hijackingRouteResponseWriter{header: make(http.Header), conn: connection},
					httptest.NewRequest(http.MethodGet, "/socket", nil))
			}()
			<-started

			closeDone := make(chan struct{})
			go func() {
				test.close(routes)
				close(closeDone)
			}()
			select {
			case <-closeDone:
			case <-time.After(time.Second):
				t.Fatal("generation retirement blocked behind a hijacked request")
			}
			select {
			case <-requestDone:
			case <-time.After(time.Second):
				t.Fatal("closing a retired hijacked connection did not drain the request")
			}
			if got := closed.Load(); got == 0 {
				t.Fatal("retired generation did not close the hijacked connection")
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("retired generation stopper did not run")
			}
		})
	}
}

type countingCloseConn struct {
	net.Conn
	closed *atomic.Int32
}

func (c *countingCloseConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

func TestRouteHandlerAbortHandlerRunsFinalizersWithoutNewMetric(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/abort", nil)
	var outcome apisixctx.ResponseOutcome
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if lifecycle == nil || !lifecycle.AddFinalizer("test", func() error {
			outcome = lifecycle.Outcome()
			return nil
		}) {
			t.Fatal("failed to register finalizer")
		}
		panic(http.ErrAbortHandler)
	})

	mustAbortHandlerPanic(t, func() { serveRouteRequest(recorder, request, handler) })
	if outcome.Kind != apisixctx.RequestOutcomeHandlerAbort {
		t.Fatalf("finalizer outcome = %#v, want handler_abort", outcome)
	}
}

func TestRouteHandlerAbortPreservesPostCommitFailureReason(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/abort", nil)
	var outcome apisixctx.ResponseOutcome
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture, ok := base.ResponseCaptureFromRequest(r)
		if !ok {
			t.Fatal("response capture is missing")
		}
		w.WriteHeader(http.StatusOK)
		capture.RecordFailure(apisixctx.ResponseFailureUpstreamIdleTimeout)
		lifecycle := apisixctx.GetRequestLifecycle(r)
		if !lifecycle.AddFinalizer("test", func() error {
			outcome = lifecycle.Outcome()
			return nil
		}) {
			t.Fatal("failed to register finalizer")
		}
		panic(http.ErrAbortHandler)
	})

	mustAbortHandlerPanic(t, func() { serveRouteRequest(recorder, request, handler) })
	if outcome.Kind != apisixctx.RequestOutcomeHandlerAbort ||
		outcome.FailureReason != apisixctx.ResponseFailureUpstreamIdleTimeout ||
		!outcome.Committed {
		t.Fatalf("finalizer outcome = %#v, want committed handler abort with idle timeout", outcome)
	}
}

func TestRouteHandlerFinalizerPanicDoesNotSkipOtherFinalizers(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	var calls []string
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		lifecycle := apisixctx.GetRequestLifecycle(r)
		for _, registration := range []struct {
			owner string
			fn    apisixctx.RequestFinalizer
		}{
			{owner: "first", fn: func() error { calls = append(calls, "first"); return nil }},
			{owner: "panic", fn: func() error { calls = append(calls, "panic"); panic("finalizer panic") }},
			{owner: "last", fn: func() error { calls = append(calls, "last"); return nil }},
		} {
			if !lifecycle.AddFinalizer(registration.owner, registration.fn) {
				t.Fatalf("failed to register %s finalizer", registration.owner)
			}
		}
		panic("application panic")
	})

	serveRouteRequest(httptest.NewRecorder(), request, handler)
	if got, want := strings.Join(calls, ","), "last,panic,first"; got != want {
		t.Fatalf("finalizer calls = %q, want %q", got, want)
	}
}

func TestRequestPanicStageUsesOnlyBoundedValues(t *testing.T) {
	tests := []struct {
		name    string
		outcome apisixctx.ResponseOutcome
		want    metrics.RequestPanicStage
	}{
		{name: "commit", outcome: apisixctx.ResponseOutcome{Committed: true}, want: metrics.RequestPanicPostCommit},
		{
			name:    "flush",
			outcome: apisixctx.ResponseOutcome{Committed: true, Flushed: true},
			want:    metrics.RequestPanicPostFlush,
		},
		{
			name: "hijack",
			outcome: apisixctx.ResponseOutcome{
				Committed: true,
				Flushed:   true,
				Hijacked:  true,
			},
			want: metrics.RequestPanicPostHijack,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestPanicStage(test.outcome); got != test.want {
				t.Fatalf("requestPanicStage(%#v) = %q, want %q", test.outcome, got, test.want)
			}
		})
	}
}

func TestRouteHandlerPanicStillReleasesRouteGeneration(t *testing.T) {
	stopped := make(chan struct{})
	routes := newRouteHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("generation panic")
	}), func() { close(stopped) })
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		defer func() { _ = recover() }()
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", nil))
	}()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("panic request did not return")
	}
	routes.Replace(http.NotFoundHandler(), nil)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retired generation was not released after panic")
	}
}

func TestRouteHandlerPanicAfterWriteAbortsConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRouteRequest(w, r, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("partial"))
			panic("abort connection")
		}))
	}))
	t.Cleanup(server.Close)

	response, err := server.Client().Get(server.URL)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			t.Fatalf("GET() error = %v, want EOF connection abort", err)
		}
		return
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr == nil {
		t.Fatalf("read body error = nil, body = %q; expected aborted connection", body)
	}
	if !bytes.HasPrefix(body, []byte("partial")) {
		t.Fatalf("body = %q, want partial prefix", body)
	}
}

func TestRouteHandlerBatchTimeoutKeepsRetiredGenerationUntilWorkerExit(t *testing.T) {
	workerStarted := make(chan struct{})
	workerRelease := make(chan struct{})
	worker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(workerStarted)
		<-workerRelease
	})
	mux := http.NewServeMux()
	mux.Handle("/slow", worker)
	mux.Handle("/apisix/batch-requests", batch_requests.NewHandlerWithLimits(mux, batch_requests.Limits{}))

	stopped := make(chan struct{})
	routes := newRouteHandler(mux, func() { close(stopped) })
	request := httptest.NewRequest(http.MethodPost, batch_requests.DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path":"/slow"}]
	}`))
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		routes.ServeHTTP(response, request)
		close(served)
	}()

	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("batch worker did not start")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("batch request did not return after timeout")
	}

	routes.Replace(http.NotFoundHandler(), nil)
	select {
	case <-stopped:
		t.Fatal("retired generation stopped while cancellation-ignoring worker was active")
	case <-time.After(100 * time.Millisecond):
	}
	close(workerRelease)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("retired generation did not stop after batch worker exited")
	}
}

func TestRetiredRouteSetRefusesNestedDispatchLease(t *testing.T) {
	generation := newRouteSet(http.NotFoundHandler(), nil)
	if !generation.acquireRequest() {
		t.Fatal("active generation refused request")
	}
	retireRouteSet(generation)
	if generation.acquireDispatchLease() {
		t.Fatal("retired generation accepted a nested dispatch lease")
	}
	generation.releaseRequest()
}

func TestRouteHandlerBatchRunsLimitConnFinalizer(t *testing.T) {
	plugin := &limit_conn.Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("limit-conn Init() error = %v", err)
	}
	config := plugin.Config().(*limit_conn.Config)
	*config = limit_conn.Config{
		Conn:             1,
		Burst:            0,
		DefaultConnDelay: 0.001,
		Key:              "remote_addr",
	}
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("limit-conn PostInit() error = %v", err)
	}

	target := plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux := http.NewServeMux()
	mux.Handle("/limited", target)
	mux.Handle("/apisix/batch-requests", batch_requests.NewHandlerWithLimits(mux, batch_requests.Limits{}))
	routes := newRouteHandler(mux, nil)
	request := httptest.NewRequest(http.MethodPost, batch_requests.DefaultURI, strings.NewReader(`{
		"pipeline": [{"path":"/limited"}, {"path":"/limited"}]
	}`))
	request.RemoteAddr = "192.0.2.100:1234"
	response := httptest.NewRecorder()

	routes.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("batch response code = %d, want 200; body=%q", response.Code, response.Body.String())
	}
	var responses []struct {
		Status int `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode batch response: %v; body=%q", err, response.Body.String())
	}
	if len(responses) != 2 || responses[0].Status != http.StatusNoContent ||
		responses[1].Status != http.StatusNoContent {
		t.Fatalf("pipeline statuses = %#v, want two 204 responses", responses)
	}
}

func TestRouteHandlerReplacementDoesNotBlockBehindOlderGeneration(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseRequest) })
	}
	t.Cleanup(release)

	firstStopped := make(chan struct{})
	first := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusOK)
	})
	routes := newRouteHandler(first, func() { close(firstStopped) })

	firstResponse := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		firstResponse <- recorder.Code
	}()
	<-requestStarted

	// Replace twice while generation 1 still has an in-flight request. Neither
	// replacement may wait for that long-lived request to drain.
	routes.Replace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	routes.Replace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), nil)

	thirdResponse := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		thirdResponse <- recorder.Code
	}()
	select {
	case status := <-thirdResponse:
		if status != http.StatusNoContent {
			t.Fatalf("third status = %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("second replacement blocked behind a retired generation")
	}

	select {
	case <-firstStopped:
		t.Fatal("first generation stopped before its in-flight request exited")
	default:
	}
	release()
	select {
	case status := <-firstResponse:
		if status != http.StatusOK {
			t.Fatalf("first status = %d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("first generation request never completed")
	}
	select {
	case <-firstStopped:
	case <-time.After(time.Second):
		t.Fatal("first generation was not stopped after its request drained")
	}
}

func TestRouteHandlerReplaceWaitsForActiveRequestBeforeStopping(t *testing.T) {
	delivered := make(chan struct{}, 1)
	processor := logger_batch.New(logger_batch.Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		delivered <- struct{}{}
		return 0, nil
	})

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var requestCount atomic.Int32
	oldHandlerCalled := make(chan struct{}, 1)
	oldHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requestCount.Add(1) == 1 {
			close(requestStarted)
			<-releaseRequest
			processor.Push(map[string]any{"path": "/old"})
		} else {
			oldHandlerCalled <- struct{}{}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	newHandlerCalled := make(chan struct{}, 1)
	newHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newHandlerCalled <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	routes := newRouteHandler(oldHandler, processor.Stop)

	requestDone := make(chan struct{})
	go func() {
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(requestDone)
	}()
	<-requestStarted

	// Replace returns immediately; the retired generation is stopped
	// asynchronously only after its active request drains.
	replaceDone := make(chan struct{})
	go func() {
		routes.Replace(newHandler, nil)
		close(replaceDone)
	}()

	select {
	case <-replaceDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Replace blocked behind the active request")
	}

	replacementRequestDone := make(chan struct{})
	go func() {
		routes.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(replacementRequestDone)
	}()
	select {
	case <-newHandlerCalled:
	case <-oldHandlerCalled:
		t.Fatal("new request reached the retired handler")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("new request blocked while the retired request was still active")
	}
	<-replacementRequestDone

	select {
	case <-delivered:
		t.Fatal("retired route logger flushed before the active request exited")
	default:
	}

	close(releaseRequest)
	<-requestDone

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for retired route logger flush")
	}
}

func TestRouteHandlerCloseStopsCurrentRoute(t *testing.T) {
	delivered := make(chan struct{}, 1)
	processor := logger_batch.New(logger_batch.Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		delivered <- struct{}{}
		return 0, nil
	})
	if !processor.Push(map[string]any{"path": "/shutdown"}) {
		t.Fatal("push was rejected")
	}

	routes := newRouteHandler(http.NotFoundHandler(), processor.Stop)
	routes.Close()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for current route logger flush")
	}
}

func TestServerShutdownStopsCurrentRoute(t *testing.T) {
	delivered := make(chan struct{}, 1)
	processor := logger_batch.New(logger_batch.Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		delivered <- struct{}{}
		return 0, nil
	})
	if !processor.Push(map[string]any{"path": "/shutdown"}) {
		t.Fatal("push was rejected")
	}

	routes := newRouteHandler(http.NotFoundHandler(), processor.Stop)
	s := &Server{server: &http.Server{}, routes: routes}
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown logger flush")
	}
}

func TestRouteHandlerStopsReplacementAfterClose(t *testing.T) {
	routes := newRouteHandler(http.NotFoundHandler(), nil)
	routes.Close()

	replacementStopped := make(chan struct{})
	replacementCalled := make(chan struct{}, 1)
	routes.Replace(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		replacementCalled <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}), func() {
		close(replacementStopped)
	})

	select {
	case <-replacementStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement route was not stopped after close")
	}

	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	select {
	case <-replacementCalled:
		t.Fatal("replacement handler was installed after close")
	default:
	}
}

func TestServerShutdownReturnsWhenHTTPQuiescenceTimesOut(t *testing.T) {
	requestStarted := make(chan struct{})
	allowLookup := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseRequest) })
	}
	t.Cleanup(release)

	events := make(chan *store.Event)
	storage, err := store.Open(
		filepath.Join(t.TempDir(), "timeout.db"),
		events,
		testutil.DataEncryptionService(false, nil),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	storage.Start()
	previousStore := store.ReplaceGlobalStoreForTest(storage)
	t.Cleanup(func() { store.ReplaceGlobalStoreForTest(previousStore) })
	t.Cleanup(func() { _ = storage.Stop() })
	lookupDone := make(chan error, 1)
	routes := newRouteHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-allowLookup
		_, lookupErr := store.GetConfigSnapshot()
		lookupDone <- lookupErr
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	httpServer := &http.Server{Handler: routes}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = httpServer.Serve(listener) }()

	requestDone := make(chan struct{})
	go func() {
		response, requestErr := http.Get("http://" + listener.Addr().String())
		if requestErr == nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()
	<-requestStarted

	s := &Server{server: httpServer, routes: routes, storage: storage}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.shutdown(shutdownCtx) }()

	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown() error = %v, want context deadline exceeded", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("shutdown did not return after its context deadline")
	}
	close(allowLookup)
	select {
	case lookupErr := <-lookupDone:
		if lookupErr != nil {
			t.Fatalf("active handler Store lookup after timeout = %v", lookupErr)
		}
	case <-time.After(time.Second):
		t.Fatal("active handler did not complete Store lookup after timeout")
	}

	release()
	<-requestDone
	if err := s.shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown() error = %v", err)
	}
	if _, err := storage.SnapshotBuckets([]string{"routes"}); err == nil {
		t.Fatal("Store remained open after completed second shutdown")
	}
}
