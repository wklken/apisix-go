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
	"github.com/wklken/apisix-go/pkg/plugin"
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
							closePanic, _ := closeRetainedGenerationConn(wrappedConnection)
							if closePanic != nil {
								panic(closePanic)
							}
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
		primary := recover()
		isHandlerAbort := primary == http.ErrAbortHandler
		pluginPanic, isPluginPanic := primary.(*plugin.PanicError)
		isUnknownCorePanic := primary != nil && !isHandlerAbort && !isPluginPanic
		var firstCoreCleanupPanic any
		outcomeBeforeCleanup := capture.Outcome()

		if bodyLimitState != nil && isUnknownCorePanic {
			bodyLimitState.disableCanonicalResponse()
		}
		bodyLimitRejected := bodyLimitState != nil && bodyLimitState.canonicalResponsePending()
		if bodyLimitState != nil && !isUnknownCorePanic {
			if recovered := captureCleanupPanic(func() {
				bodyLimitState.writeCanonicalResponse(wrapped, request)
			}); recovered != nil {
				bodyLimitState.disableCanonicalResponse()
				firstCoreCleanupPanic = recovered
			}
		}
		outcome := capture.Outcome()
		pluginPostCommitAbort := false

		switch {
		case primary == nil:
			outcome.Kind = apisixctx.RequestOutcomeCompleted
		case isHandlerAbort:
			outcome.Kind = apisixctx.RequestOutcomeHandlerAbort
		case isPluginPanic && bodyLimitRejected:
			logger.Errorf("recovered %s after body-limit rejection\n%s", pluginPanic, pluginPanic.Stack)
			metrics.RecordRequestPanic(metrics.RequestPanicPlugin, metrics.RequestPanicPreCommit)
			outcome.Kind = apisixctx.RequestOutcomeRecoveredPanic
		case isPluginPanic:
			logger.Errorf("recovered %s\n%s", pluginPanic, pluginPanic.Stack)
			apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
			if outcome.Committed || outcome.Flushed || outcome.Hijacked {
				metrics.RecordRequestPanic(metrics.RequestPanicPlugin, requestPanicStage(outcome))
				outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
				pluginPostCommitAbort = true
			} else {
				metrics.RecordRequestPanic(metrics.RequestPanicPlugin, metrics.RequestPanicPreCommit)
				ok, writerPanic := writeStableInternalError(wrapped)
				if writerPanic != nil && firstCoreCleanupPanic == nil {
					firstCoreCleanupPanic = writerPanic
				}
				outcome = capture.Outcome()
				outcome.Kind = apisixctx.RequestOutcomeRecoveredPanic
				if !ok {
					outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
					pluginPostCommitAbort = writerPanic == nil
				}
			}
		case isUnknownCorePanic:
			logger.Errorf("request core invariant panic: %v\n%s", primary, debug.Stack())
			metrics.RecordRequestPanic(metrics.RequestPanicCore, requestPanicStageBeforeCommit(outcomeBeforeCleanup))
			outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
		}
		if firstCoreCleanupPanic != nil {
			metrics.RecordRequestPanic(metrics.RequestPanicCore, requestPanicStageBeforeCommit(outcome))
			outcome.Kind = apisixctx.RequestOutcomeAbortedPanic
		}

		lifecycle.Complete(outcome, time.Now())
		finalization := lifecycle.FinalizeResult()
		for _, failure := range finalization.Failures {
			logFinalizerFailure(failure)
			if owner, ok := finalizerPanicOwner(failure); ok {
				metrics.RecordRequestPanic(owner, metrics.RequestPanicFinalizer)
			}
		}
		if recovered := captureCleanupPanic(
			func() { apisixctx.RecycleVars(request) },
		); recovered != nil {
			metrics.RecordRequestPanic(metrics.RequestPanicCore, metrics.RequestPanicFinalizer)
			if firstCoreCleanupPanic == nil {
				firstCoreCleanupPanic = recovered
			}
		}

		mustAbort := primary != nil || firstCoreCleanupPanic != nil || finalization.FatalPanic != nil
		if outcome.Hijacked && mustAbort {
			if len(generationHijacks) != 0 {
				for _, connection := range generationHijacks {
					closePanic, closeErr := closeRetainedGenerationConn(connection)
					if closeErr != nil {
						logger.Errorf("close hijacked request connection: %s", closeErr)
					}
					if closePanic != nil {
						metrics.RecordRequestPanic(metrics.RequestPanicCore, metrics.RequestPanicPostHijack)
						if firstCoreCleanupPanic == nil {
							firstCoreCleanupPanic = closePanic
						}
					}
				}
			} else if closePanic := captureCleanupPanic(func() {
				if err := capture.CloseHijacked(); err != nil {
					logger.Errorf("close hijacked request connection: %s", err)
				}
			}); closePanic != nil {
				metrics.RecordRequestPanic(metrics.RequestPanicCore, metrics.RequestPanicPostHijack)
				if firstCoreCleanupPanic == nil {
					firstCoreCleanupPanic = closePanic
				}
			}
		}
		if isUnknownCorePanic {
			panic(primary)
		}
		if firstCoreCleanupPanic != nil {
			panic(firstCoreCleanupPanic)
		}
		if finalization.FatalPanic != nil {
			panic(finalization.FatalPanic.PanicValue)
		}
		if pluginPostCommitAbort {
			panic(http.ErrAbortHandler)
		}
		if isHandlerAbort {
			panic(primary)
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

func requestPanicStageBeforeCommit(outcome apisixctx.ResponseOutcome) metrics.RequestPanicStage {
	if !outcome.Committed && !outcome.Flushed && !outcome.Hijacked {
		return metrics.RequestPanicPreCommit
	}
	return requestPanicStage(outcome)
}

func captureCleanupPanic(call func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	call()
	return nil
}

func closeRetainedGenerationConn(connection *generationConn) (panicValue any, closeErr error) {
	if connection == nil {
		return nil, nil
	}
	panicValue = captureCleanupPanic(func() { closeErr = connection.Close() })
	return panicValue, closeErr
}

func writeStableInternalError(w http.ResponseWriter) (ok bool, panicValue any) {
	panicValue = captureCleanupPanic(func() {
		for key := range w.Header() {
			w.Header().Del(key)
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		w.WriteHeader(http.StatusInternalServerError)
		const body = `{"message":"Internal Server Error"}`
		written, err := w.Write([]byte(body))
		ok = err == nil && written == len(body)
	})
	return ok, panicValue
}

func finalizerPanicOwner(failure apisixctx.FinalizerFailure) (metrics.RequestPanicOwner, bool) {
	var pluginPanic *plugin.PanicError
	if errors.As(failure.Err, &pluginPanic) {
		return metrics.RequestPanicPluginFinalizer, true
	}
	if failure.PanicValue == nil {
		return "", false
	}
	if failure.Kind == apisixctx.FinalizerOwnerCoreInvariant {
		return metrics.RequestPanicCoreFinalizer, true
	}
	return metrics.RequestPanicPluginFinalizer, true
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
	var firstClosePanic any
	for _, connection := range connections {
		closePanic, _ := closeRetainedGenerationConn(connection)
		if firstClosePanic == nil {
			firstClosePanic = closePanic
		}
	}
	if firstClosePanic != nil {
		panic(firstClosePanic)
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
		closePanic, _ := closeRetainedGenerationConn(wrapped)
		if closePanic != nil {
			panic(closePanic)
		}
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
