package ai_stream

import (
	"net/http"
	"sync"
	"time"
)

type FlushWriter struct {
	writer   http.ResponseWriter
	interval time.Duration
	onFirst  func()

	mu        sync.Mutex
	pending   bool
	wrote     bool
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func NewFlushWriter(writer http.ResponseWriter, interval time.Duration, onFirst func()) *FlushWriter {
	flushWriter := &FlushWriter{
		writer: writer, interval: interval, onFirst: onFirst,
	}
	if interval > 0 {
		flushWriter.stop = make(chan struct{})
		flushWriter.done = make(chan struct{})
		go flushWriter.flushLoop()
	}
	return flushWriter
}

func (w *FlushWriter) Header() http.Header {
	return w.writer.Header()
}

func (w *FlushWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	w.writer.WriteHeader(statusCode)
	w.mu.Unlock()
}

func (w *FlushWriter) Write(body []byte) (int, error) {
	w.mu.Lock()
	first := !w.wrote
	w.wrote = true
	w.pending = true
	written, err := w.writer.Write(body)
	w.mu.Unlock()
	if first && w.onFirst != nil {
		w.onFirst()
	}
	return written, err
}

func (w *FlushWriter) Flush() {
	w.mu.Lock()
	if w.interval <= 0 {
		w.flushLocked()
	} else {
		w.pending = true
	}
	w.mu.Unlock()
}

func (w *FlushWriter) Close() {
	w.closeOnce.Do(func() {
		if w.stop != nil {
			close(w.stop)
			<-w.done
			return
		}
		w.mu.Lock()
		w.flushLocked()
		w.mu.Unlock()
	})
}

func (w *FlushWriter) flushLoop() {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	defer close(w.done)
	for {
		select {
		case <-ticker.C:
			w.mu.Lock()
			w.flushLocked()
			w.mu.Unlock()
		case <-w.stop:
			w.mu.Lock()
			w.flushLocked()
			w.mu.Unlock()
			return
		}
	}
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
