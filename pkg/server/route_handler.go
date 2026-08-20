package server

import (
	"bufio"
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
	mu      sync.Mutex
	current atomic.Pointer[routeSet]
	closed  bool
}

func newRouteHandler(handler http.Handler, stop func()) *routeHandler {
	routes := &routeHandler{}
	routes.current.Store(newRouteSet(handler, stop))
	return routes
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func serveRouteRequest(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	serveRouteRequestForGeneration(w, r, handler, nil)
}

func serveRouteRequestForGeneration(
	w http.ResponseWriter,
	r *http.Request,
	handler http.Handler,
	generation *routeSet,
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
	if generation != nil {
		wrapped = httpsnoop.Wrap(wrapped, httpsnoop.Hooks{
			Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
				return func() (net.Conn, *bufio.ReadWriter, error) {
					connection, readWriter, err := hijack()
					if err == nil && connection != nil {
						unregisterHijacks = append(unregisterHijacks, generation.registerHijacked(connection))
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
	if generation != nil {
		request = batch_requests.WithDispatchLeaseFactory(request, func() (batch_requests.DispatchLease, bool) {
			if !generation.acquireDispatchLease() {
				return batch_requests.DispatchLease{}, false
			}
			var releaseOnce sync.Once
			return batch_requests.DispatchLease{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serveRouteRequestForGeneration(w, r, generation.handler, generation)
				}),
				Release: func() {
					releaseOnce.Do(generation.releaseRequest)
				},
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
			if err := capture.CloseHijacked(); err != nil {
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
	previous := h.current.Swap(nil)
	retireRouteSet(previous)
	h.mu.Unlock()

	stopRouteSet(previous)
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
		if state == routeSetRetired {
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
