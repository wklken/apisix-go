package ai_stream

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/runtime"
)

type FlushWriter struct {
	writer   http.ResponseWriter
	interval time.Duration
	onFirst  func()

	mu        sync.Mutex
	pending   bool
	wrote     bool
	status    int
	stop      chan struct{}
	tasks     *runtime.RequestTaskGroup
	closeOnce sync.Once
	closeMu   sync.Mutex
	panicked  bool
	panic     any
}

func NewFlushWriter(
	ctx context.Context,
	writer http.ResponseWriter,
	interval time.Duration,
	onFirst func(),
) *FlushWriter {
	flushWriter := &FlushWriter{
		writer: writer, interval: interval, onFirst: onFirst,
	}
	if interval > 0 {
		flushWriter.stop = make(chan struct{})
		flushWriter.tasks = runtime.NewRequestTaskGroup(ctx, "request/ai-stream")
		if err := flushWriter.tasks.Go(func(taskCtx context.Context) error {
			flushWriter.flushLoop(taskCtx)
			return nil
		}); err != nil {
			panic(err)
		}
	}
	return flushWriter
}

func (w *FlushWriter) Header() http.Header {
	return w.writer.Header()
}

func (w *FlushWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = statusCode
}

// Status returns the pending response status, or 0 when no status was set.
func (w *FlushWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// Wrote reports whether any body bytes have been written.
func (w *FlushWriter) Wrote() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wrote
}

func (w *FlushWriter) Write(body []byte) (int, error) {
	first := false
	written, err := func() (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		first = !w.wrote
		w.wrote = true
		w.pending = true
		if first && w.status != 0 {
			w.writer.WriteHeader(w.status)
		}
		return w.writer.Write(body)
	}()
	if first && w.onFirst != nil {
		w.onFirst()
	}
	return written, err
}

func (w *FlushWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.interval <= 0 {
		w.flushLocked()
	} else {
		w.pending = true
	}
}

func (w *FlushWriter) Close() {
	w.closeOnce.Do(func() {
		panicked, panicValue := func() (panicked bool, panicValue any) {
			defer func() {
				if recovered := recover(); recovered != nil {
					panicked = true
					panicValue = recovered
				}
			}()
			if w.stop != nil {
				close(w.stop)
				_ = w.tasks.Wait()
				// The context-owned loop may have exited before the request
				// finished its last write. Flush once more after the join; the
				// pending bit keeps the normal stop/cancel path idempotent.
				w.flush()
				return false, nil
			}
			w.Flush()
			return false, nil
		}()
		w.closeMu.Lock()
		w.panicked = panicked
		w.panic = panicValue
		w.closeMu.Unlock()
	})
	w.closeMu.Lock()
	panicked := w.panicked
	panicValue := w.panic
	w.closeMu.Unlock()
	if panicked {
		panic(panicValue)
	}
}

// ClosePreservingPanic must be called directly from defer. It always joins the
// writer, but an existing request-stack panic takes precedence over a cleanup
// panic raised by Close.
func ClosePreservingPanic(w *FlushWriter) {
	primary := recover()
	closePanicked := false
	var closePanic any
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePanicked = true
				closePanic = recovered
			}
		}()
		w.Close()
	}()
	if primary != nil {
		panic(primary)
	}
	if closePanicked {
		panic(closePanic)
	}
}

func (w *FlushWriter) flushLoop(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.flush()
		case <-ctx.Done():
			w.flush()
			return
		case <-w.stop:
			w.flush()
			return
		}
	}
}

func (w *FlushWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushLocked()
}

func (w *FlushWriter) flushLocked() {
	if !w.pending {
		return
	}
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	w.pending = false
}
