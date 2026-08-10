package server

import (
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const routeSetRetired = uint64(1) << 63

type routeSet struct {
	handler     http.Handler
	stop        func()
	state       atomic.Uint64 // high bit is retired; remaining bits are active requests
	drained     chan struct{}
	drainedOnce sync.Once
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
		state := current.state.Load()
		if state&routeSetRetired != 0 {
			continue
		}
		if current.state.CompareAndSwap(state, state+1) {
			break
		}
	}

	defer h.finishRequest(current)
	serveRouteRequest(w, r, current.handler)
}

func serveRouteRequest(w http.ResponseWriter, r *http.Request, handler http.Handler) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(r, time.Now())
	wrapped, snapshot, closeHijacked := base.CaptureResponseOutcome(w)

	defer func() {
		recovered := recover()
		outcome := snapshot()
		aborted := false
		isHandlerAbort := recovered == http.ErrAbortHandler

		switch {
		case recovered == nil:
			outcome.Kind = apisixctx.RequestOutcomeCompleted
		case isHandlerAbort:
			outcome.Kind = apisixctx.RequestOutcomeHandlerAbort
		default:
			logger.Errorf("recovered request panic: %v\n%s", recovered, debug.Stack())
			if outcome.Committed || outcome.Flushed || outcome.Hijacked {
				metrics.RecordRequestPanic(requestPanicStage(outcome))
				outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
				aborted = true
			} else {
				metrics.RecordRequestPanic(metrics.RequestPanicPreCommit)
				if !writeStableInternalError(wrapped) {
					outcome = snapshot()
					aborted = true
				} else {
					outcome = snapshot()
				}
				outcome.Kind = apisixctx.RequestOutcomeRecoveredPanic
				if aborted {
					outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
				}
			}
		}

		lifecycle.SetOutcome(outcome)
		for _, failure := range lifecycle.Finalize() {
			logFinalizerFailure(failure)
			if failure.PanicValue != nil {
				metrics.RecordRequestPanic(metrics.RequestPanicFinalizer)
			}
		}
		apisixctx.RecycleVars(request)

		if outcome.Hijacked && (isHandlerAbort || aborted) {
			if err := closeHijacked(); err != nil {
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

func (h *routeHandler) finishRequest(current *routeSet) {
	state := current.state.Add(^uint64(0))
	if state == routeSetRetired {
		current.closeDrained()
	}
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
			if retired == routeSetRetired {
				current.closeDrained()
			}
			return
		}
	}
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

// retireAndStopRouteSet retires and stops an old route generation without
// blocking the caller. Publication must never wait for a long-lived request
// on a replaced generation; the stopper still runs only after that request
// drains. It is used only from Replace.
func retireAndStopRouteSet(current *routeSet) {
	if current == nil {
		return
	}
	go stopRouteSet(current)
}
