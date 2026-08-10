package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/store"
)

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
