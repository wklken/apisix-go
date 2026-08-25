package server

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
)

var errHTTPGenerationHijackUnavailable = errors.New("HTTP generation hijack lease unavailable")

const routeSetRetired = uint64(1) << 63

type routeSet struct {
	handler     http.Handler
	stop        func()
	state       atomic.Uint64 // high bit is retired; remaining bits are requests and batch leases
	drained     chan struct{}
	drainedOnce sync.Once
	hijackMu    sync.Mutex
	hijacked    map[net.Conn]struct{}
	hijackOnce  sync.Once
}

type routeHandler struct {
	mu               sync.Mutex
	current          atomic.Pointer[routeSet]
	generation       bool
	generationSource httpLeaseSource
	hijacked         map[*generationConn]struct{}
	closed           bool
}

func newRouteHandler(handler http.Handler, stop func()) *routeHandler {
	routes := &routeHandler{}
	routes.current.Store(newRouteSet(handler, stop))
	return routes
}

func newGenerationRouteHandler(source httpLeaseSource) *routeHandler {
	return &routeHandler{
		generation:       true,
		generationSource: source,
		hijacked:         make(map[*generationConn]struct{}),
	}
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.generation {
		h.serveGeneration(w, r)
		return
	}
	var current *routeSet
	for {
		current = h.current.Load()
		if current == nil || current.handler == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		if current.acquireRequest() {
			break
		}
	}

	defer h.finishRequest(current)
	serveRouteRequestForGeneration(w, r, current.handler, current)
}

func (h *routeHandler) serveGeneration(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closed || h.generationSource == nil {
		h.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	lease, ok := h.generationSource()
	h.mu.Unlock()
	if !ok || lease.Snapshot == nil || lease.Snapshot.Handler() == nil || lease.Release == nil {
		if ok && lease.Release != nil {
			lease.Release()
		}
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer lease.Release()
	serveRouteRequestForHTTPGeneration(w, r, lease.Snapshot.Handler(), &lease, h)
}

func serveRouteRequest(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	serveRouteRequestForGeneration(w, r, handler, nil)
}

func serveRouteRequestForGeneration(
	w http.ResponseWriter,
	r *http.Request,
	handler http.Handler,
	generation *routeSet,
) {
	serveRouteRequestForOwnedGeneration(w, r, handler, generation, nil, nil)
}

func serveRouteRequestForHTTPGeneration(
	w http.ResponseWriter,
	r *http.Request,
	handler http.Handler,
	lease *httpGenerationLease,
	terminal *routeHandler,
) {
	serveRouteRequestForOwnedGeneration(w, r, handler, nil, lease, terminal)
}

func serveRouteRequestForOwnedGeneration(
	w http.ResponseWriter,
	r *http.Request,
	handler http.Handler,
	legacy *routeSet,
	lease *httpGenerationLease,
	terminal *routeHandler,
) {
	r.Header.Del("X-Consumer-Username")
	request, lifecycle := apisixctx.EnsureRequestLifecycle(r, time.Now())
	bodyLimitState := requestBodyLimitStateFromRequest(request)
	captureWriter := w
	if bodyLimitState != nil {
		captureWriter = bodyLimitState.responseWriter
	}
	wrapped, capture := base.CaptureResponseOutcomeController(captureWriter)
	if bodyLimitState != nil {
		wrapped = bodyLimitState.wrapResponseWriter(wrapped)
	}
	var unregisterHijacks []func()
	var generationHijacks []*generationConn
	if legacy != nil || lease != nil {
		wrapped = httpsnoop.Wrap(wrapped, httpsnoop.Hooks{
			Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
				return func() (net.Conn, *bufio.ReadWriter, error) {
					connection, readWriter, err := hijack()
					if err == nil && connection != nil {
						if legacy != nil {
							unregisterHijacks = append(unregisterHijacks, legacy.registerHijacked(connection))
						} else {
							wrappedConnection, wrapErr := terminal.registerGenerationHijack(connection, lease)
							if wrapErr != nil {
								return nil, nil, wrapErr
							}
							generationHijacks = append(generationHijacks, wrappedConnection)
							connection = wrappedConnection
							readWriter, wrapErr = rebuildGenerationReadWriter(readWriter, wrappedConnection)
							if wrapErr != nil {
								_ = wrappedConnection.Close()
								return nil, nil, wrapErr
							}
						}
					}
					return connection, readWriter, err
				}
			},
		})
	}
	defer func() {
		for _, unregister := range unregisterHijacks {
			unregister()
		}
	}()
	request = base.WithResponseCapture(request, capture)
	if legacy != nil {
		request = batch_requests.WithDispatchLeaseFactory(request, func() (batch_requests.DispatchLease, bool) {
			if !legacy.acquireDispatchLease() {
				return batch_requests.DispatchLease{}, false
			}
			var releaseOnce sync.Once
			return batch_requests.DispatchLease{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serveRouteRequestForGeneration(w, r, legacy.handler, legacy)
				}),
				Release: func() {
					releaseOnce.Do(legacy.releaseRequest)
				},
			}, true
		})
	} else if lease != nil && lease.retain != nil {
		request = batch_requests.WithDispatchLeaseFactory(request, func() (batch_requests.DispatchLease, bool) {
			child, ok := lease.retain()
			if !ok || child.Snapshot == nil || child.Snapshot.Handler() == nil {
				if ok && child.Release != nil {
					child.Release()
				}
				return batch_requests.DispatchLease{}, false
			}
			return batch_requests.DispatchLease{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serveRouteRequestForHTTPGeneration(w, r, child.Snapshot.Handler(), &child, terminal)
				}),
				Release: child.Release,
			}, true
		})
	}
	lifecycle.SetFinalRequest(request)

	defer func() {
		recovered := recover()
		bodyLimitFinalized := false
		if bodyLimitState != nil {
			bodyLimitFinalized = bodyLimitState.writeCanonicalResponse(wrapped, request)
		}
		outcome := capture.Outcome()
		aborted := false
		isHandlerAbort := recovered == http.ErrAbortHandler

		switch {
		case recovered == nil:
			outcome.Kind = apisixctx.RequestOutcomeCompleted
		case bodyLimitFinalized:
			logger.Errorf("recovered request panic after body-limit rejection: %v\n%s", recovered, debug.Stack())
			metrics.RecordRequestPanic(metrics.RequestPanicPreCommit)
			outcome.Kind = apisixctx.RequestOutcomeRecoveredPanic
		case isHandlerAbort:
			outcome.Kind = apisixctx.RequestOutcomeHandlerAbort
		default:
			logger.Errorf("recovered request panic: %v\n%s", recovered, debug.Stack())
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
			if outcome.Committed || outcome.Flushed || outcome.Hijacked {
				metrics.RecordRequestPanic(requestPanicStage(outcome))
				outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
				aborted = true
			} else {
				metrics.RecordRequestPanic(metrics.RequestPanicPreCommit)
				if !writeStableInternalError(wrapped) {
					outcome = capture.Outcome()
					aborted = true
				} else {
					outcome = capture.Outcome()
				}
				outcome.Kind = apisixctx.RequestOutcomeRecoveredPanic
				if aborted {
					outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
				}
			}
		}

		lifecycle.Complete(outcome, time.Now())
		for _, failure := range lifecycle.Finalize() {
			logFinalizerFailure(failure)
			if failure.PanicValue != nil {
				metrics.RecordRequestPanic(metrics.RequestPanicFinalizer)
			}
		}
		apisixctx.RecycleVars(request)

		if outcome.Hijacked && (isHandlerAbort || aborted) {
			if len(generationHijacks) != 0 {
				for _, connection := range generationHijacks {
					if err := connection.Close(); err != nil {
						logger.Errorf("close hijacked request connection: %s", err)
					}
				}
			} else if err := capture.CloseHijacked(); err != nil {
				logger.Errorf("close hijacked request connection: %s", err)
			}
		}
		if isHandlerAbort {
			panic(recovered)
		}
		if aborted {
			panic(http.ErrAbortHandler)
		}
	}()

	handler.ServeHTTP(wrapped, request)
}

func rebuildGenerationReadWriter(
	readWriter *bufio.ReadWriter,
	connection *generationConn,
) (*bufio.ReadWriter, error) {
	if connection == nil {
		return nil, errHTTPGenerationHijackUnavailable
	}
	var buffered []byte
	if readWriter != nil {
		writer := readWriter.Writer
		if writer != nil {
			if err := writer.Flush(); err != nil {
				return nil, fmt.Errorf("flush hijacked response before generation wrap: %w", err)
			}
		}
		reader := readWriter.Reader
		if reader != nil && reader.Buffered() != 0 {
			peeked, err := reader.Peek(reader.Buffered())
			if err != nil {
				return nil, fmt.Errorf("preserve hijacked buffered input: %w", err)
			}
			buffered = bytes.Clone(peeked)
		}
	}
	reader := io.Reader(connection)
	if len(buffered) != 0 {
		reader = io.MultiReader(bytes.NewReader(buffered), connection)
	}
	return bufio.NewReadWriter(bufio.NewReader(reader), bufio.NewWriter(connection)), nil
}

func requestPanicStage(outcome apisixctx.ResponseOutcome) metrics.RequestPanicStage {
	if outcome.Hijacked {
		return metrics.RequestPanicPostHijack
	}
	if outcome.Flushed {
		return metrics.RequestPanicPostFlush
	}
	return metrics.RequestPanicPostCommit
}

func writeStableInternalError(w http.ResponseWriter) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()

	for key := range w.Header() {
		w.Header().Del(key)
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusInternalServerError)
	const body = `{"message":"Internal Server Error"}`
	written, err := w.Write([]byte(body))
	return err == nil && written == len(body)
}

func logFinalizerFailure(failure apisixctx.FinalizerFailure) {
	if failure.PanicValue != nil {
		logger.Errorf("request finalizer %q panicked: %v\n%s", failure.Owner, failure.PanicValue, failure.Stack)
		return
	}
	if failure.Err != nil {
		logger.Errorf("request finalizer %q failed: %s", failure.Owner, failure.Err)
	}
}

func (h *routeHandler) Replace(handler http.Handler, stop func()) {
	next := newRouteSet(handler, stop)
	h.mu.Lock()
	if h.closed {
		retireRouteSet(next)
		h.mu.Unlock()
		stopRouteSet(next)
		return
	}
	previous := h.current.Swap(next)
	retireRouteSet(previous)
	h.mu.Unlock()

	retireAndStopRouteSet(previous)
}

func (h *routeHandler) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	if h.generationSource != nil {
		h.generationSource = nil
		connections := make([]*generationConn, 0, len(h.hijacked))
		for connection := range h.hijacked {
			connections = append(connections, connection)
		}
		clear(h.hijacked)
		h.mu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		return
	}
	previous := h.current.Swap(nil)
	retireRouteSet(previous)
	h.mu.Unlock()

	stopRouteSet(previous)
}

func (h *routeHandler) registerGenerationHijack(
	connection net.Conn,
	lease *httpGenerationLease,
) (*generationConn, error) {
	if h == nil || connection == nil || lease == nil || lease.retain == nil {
		if connection != nil {
			_ = connection.Close()
		}
		return nil, errHTTPGenerationHijackUnavailable
	}
	hijackLease, ok := lease.retain()
	if !ok {
		_ = connection.Close()
		return nil, errHTTPGenerationHijackUnavailable
	}
	wrapped := newGenerationConn(connection, hijackLease.Release, nil)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = wrapped.Close()
		return nil, errHTTPGenerationHijackUnavailable
	}
	wrapped.unregister = func() {
		h.mu.Lock()
		delete(h.hijacked, wrapped)
		h.mu.Unlock()
	}
	h.hijacked[wrapped] = struct{}{}
	h.mu.Unlock()
	return wrapped, nil
}

func newRouteSet(handler http.Handler, stop func()) *routeSet {
	return &routeSet{handler: handler, stop: stop, drained: make(chan struct{})}
}

func (r *routeSet) acquireRequest() bool {
	for {
		state := r.state.Load()
		if state&routeSetRetired != 0 {
			return false
		}
		if r.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

func (r *routeSet) acquireDispatchLease() bool {
	for {
		state := r.state.Load()
		if state&routeSetRetired != 0 {
			return false
		}
		if r.state.CompareAndSwap(state, state+1) {
			return true
		}
	}
}

func (r *routeSet) releaseRequest() {
	state := r.state.Add(^uint64(0))
	if state == routeSetRetired {
		r.closeDrained()
	}
}

func (h *routeHandler) finishRequest(current *routeSet) {
	current.releaseRequest()
}

func retireRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	for {
		state := current.state.Load()
		if state&routeSetRetired != 0 {
			return
		}
		retired := state | routeSetRetired
		if current.state.CompareAndSwap(state, retired) {
			current.closeHijacked()
			if retired == routeSetRetired {
				current.closeDrained()
			}
			return
		}
	}
}

func (r *routeSet) registerHijacked(connection net.Conn) func() {
	if connection == nil {
		return func() {}
	}
	r.hijackMu.Lock()
	if r.state.Load()&routeSetRetired != 0 {
		r.hijackMu.Unlock()
		_ = connection.Close()
		return func() {}
	}
	if r.hijacked == nil {
		r.hijacked = make(map[net.Conn]struct{})
	}
	r.hijacked[connection] = struct{}{}
	r.hijackMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			r.hijackMu.Lock()
			delete(r.hijacked, connection)
			r.hijackMu.Unlock()
		})
	}
}

func (r *routeSet) closeHijacked() {
	r.hijackOnce.Do(func() {
		r.hijackMu.Lock()
		connections := make([]net.Conn, 0, len(r.hijacked))
		for connection := range r.hijacked {
			connections = append(connections, connection)
		}
		clear(r.hijacked)
		r.hijackMu.Unlock()
		for _, connection := range connections {
			_ = connection.Close()
		}
	})
}

func (r *routeSet) closeDrained() {
	r.drainedOnce.Do(func() { close(r.drained) })
}

func stopRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	<-current.drained
	if current.stop != nil {
		current.stop()
	}
}

// retireAndStopRouteSet retires and stops a replaced route generation
// asynchronously so replacement publication does not block on long-lived
// requests. routeHandler.Close uses stopRouteSet directly and remains
// synchronous.
func retireAndStopRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	go stopRouteSet(current)
}
