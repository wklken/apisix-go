package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
)

var errHTTPGenerationHijackUnavailable = errors.New("HTTP generation hijack lease unavailable")

type routeHandler struct {
	mu                  sync.Mutex
	generationSource    httpLeaseSource
	hijacked            map[*generationConn]struct{}
	generationActive    int
	generationDrained   chan struct{}
	generationDrainOnce sync.Once
	closed              bool
}

func newGenerationRouteHandler(source httpLeaseSource) *routeHandler {
	return &routeHandler{
		generationSource:  source,
		hijacked:          make(map[*generationConn]struct{}),
		generationDrained: make(chan struct{}),
	}
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.serveGeneration(w, r)
}

func (h *routeHandler) serveGeneration(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closed || h.generationSource == nil {
		h.mu.Unlock()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	lease, ok := h.generationSource()
	if !ok || lease.Snapshot == nil || lease.Snapshot.Handler() == nil || lease.Release == nil {
		h.mu.Unlock()
		if ok && lease.Release != nil {
			lease.Release()
		}
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	h.generationActive++
	h.mu.Unlock()
	defer h.finishGenerationLease(lease.Release)
	serveRouteRequestForHTTPGeneration(w, r, lease.Snapshot.Handler(), &lease, h)
}

func serveRouteRequestForHTTPGeneration(
	w http.ResponseWriter,
	r *http.Request,
	handler http.Handler,
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
	var generationHijacks []*generationConn
	if lease != nil {
		wrapped = httpsnoop.Wrap(wrapped, httpsnoop.Hooks{
			Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
				return func() (net.Conn, *bufio.ReadWriter, error) {
					connection, readWriter, err := hijack()
					if err == nil && connection != nil {
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
					return connection, readWriter, err
				}
			},
		})
	}
	request = base.WithResponseCapture(request, capture)
	if lease != nil && lease.retain != nil {
		request = batch_requests.WithDispatchLeaseFactory(request, func() (batch_requests.DispatchLease, bool) {
			var child httpGenerationLease
			var ok bool
			if terminal == nil {
				child, ok = lease.retain()
			} else {
				child, ok = terminal.retainGenerationLease(lease)
			}
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

func (h *routeHandler) Close() {
	h.RejectNew()
	_ = h.Drain(context.Background())
}

func (h *routeHandler) RejectNew() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	h.generationSource = nil
	connections := make([]*generationConn, 0, len(h.hijacked))
	for connection := range h.hijacked {
		connections = append(connections, connection)
	}
	clear(h.hijacked)
	h.signalGenerationDrainedLocked()
	h.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (h *routeHandler) Drain(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.RejectNew()
	h.mu.Lock()
	drained := h.generationDrained
	h.mu.Unlock()
	if drained == nil {
		return nil
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *routeHandler) retainGenerationLease(parent *httpGenerationLease) (httpGenerationLease, bool) {
	if h == nil || parent == nil || parent.retain == nil {
		return httpGenerationLease{}, false
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return httpGenerationLease{}, false
	}
	child, ok := parent.retain()
	if !ok || child.Release == nil {
		h.mu.Unlock()
		if ok && child.Release != nil {
			child.Release()
		}
		return httpGenerationLease{}, false
	}
	h.generationActive++
	release := child.Release
	child.Release = func() { h.finishGenerationLease(release) }
	h.mu.Unlock()
	return child, true
}

func (h *routeHandler) finishGenerationLease(release func()) {
	if release != nil {
		release()
	}
	h.mu.Lock()
	if h.generationActive > 0 {
		h.generationActive--
	}
	h.signalGenerationDrainedLocked()
	h.mu.Unlock()
}

func (h *routeHandler) signalGenerationDrainedLocked() {
	if !h.closed || h.generationActive != 0 || h.generationDrained == nil {
		return
	}
	h.generationDrainOnce.Do(func() { close(h.generationDrained) })
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
	hijackLease, ok := h.retainGenerationLease(lease)
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
