package server

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/store"
)

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

func (w *hijackingRouteResponseWriter) Header() http.Header { return w.header }

func (w *hijackingRouteResponseWriter) WriteHeader(status int) { w.writeStatus = status }

func (w *hijackingRouteResponseWriter) Write(body []byte) (int, error) { return len(body), nil }

func (w *hijackingRouteResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
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
	storage, err := store.Open(filepath.Join(t.TempDir(), "timeout.db"), events)
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
