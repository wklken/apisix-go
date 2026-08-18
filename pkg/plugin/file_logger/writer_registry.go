package file_logger

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"go.uber.org/zap/zapcore"
)

const (
	fileLoggerBufferSize    = 64 * 1024
	fileLoggerFlushInterval = time.Second
)

var errFileLoggerWriterStopped = errors.New("file logger writer is stopped")

type registeredFileWriter struct {
	writer *bufferedFileWriteSyncer
	leases int
}

type fileWriterRegistry struct {
	mu      sync.Mutex
	writers map[string]*registeredFileWriter
	signals chan os.Signal
	stop    chan struct{}
	done    chan struct{}
}

type fileWriterLease struct {
	registry *fileWriterRegistry
	path     string
	writer   *bufferedFileWriteSyncer
	once     sync.Once
}

// bufferedFileWriteSyncer owns the buffered sink for one canonical file path.
// The wrapper lock serializes writes with explicit reopen and final stop
// operations, while BufferedWriteSyncer serializes its periodic flushes with
// its own lock.
type bufferedFileWriteSyncer struct {
	mu      sync.Mutex
	raw     *appendFileWriteSyncer
	buffer  *zapcore.BufferedWriteSyncer
	stopped bool
}

func newBufferedFileWriteSyncer(path string) *bufferedFileWriteSyncer {
	raw := &appendFileWriteSyncer{path: path}
	return &bufferedFileWriteSyncer{
		raw:    raw,
		buffer: newFileLoggerBufferedWriteSyncer(raw),
	}
}

func newFileLoggerBufferedWriteSyncer(raw *appendFileWriteSyncer) *zapcore.BufferedWriteSyncer {
	return &zapcore.BufferedWriteSyncer{
		WS:            raw,
		Size:          fileLoggerBufferSize,
		FlushInterval: fileLoggerFlushInterval,
	}
}

// resetBufferLocked discards a buffer after a write or sync error. bufio.Writer
// keeps its first underlying error forever, so retaining the old
// BufferedWriteSyncer would make every later entry fail even after the file
// becomes available again.
func (w *bufferedFileWriteSyncer) resetBufferLocked() {
	old := w.buffer
	_ = old.Stop()
	w.buffer = newFileLoggerBufferedWriteSyncer(w.raw)
}

func (w *bufferedFileWriteSyncer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return 0, errFileLoggerWriterStopped
	}
	n, err := w.buffer.Write(data)
	if err != nil {
		w.resetBufferLocked()
		// A sticky error or a failed flush of previously buffered bytes
		// returns n == 0, so the current entry is safe to retry. Never retry
		// after a partial write because that could duplicate its prefix.
		if n == 0 {
			return w.buffer.Write(data)
		}
	}
	return n, err
}

func (w *bufferedFileWriteSyncer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return nil
	}
	err := w.buffer.Sync()
	if err != nil {
		w.resetBufferLocked()
	}
	return err
}

func (w *bufferedFileWriteSyncer) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return errFileLoggerWriterStopped
	}
	syncErr := w.buffer.Sync()
	if syncErr != nil {
		w.resetBufferLocked()
	}
	return errors.Join(syncErr, w.raw.Reopen())
}

func (w *bufferedFileWriteSyncer) Stop() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return nil
	}
	w.stopped = true
	return errors.Join(w.buffer.Stop(), w.raw.Close())
}

var sharedFileWriters = &fileWriterRegistry{
	writers: make(map[string]*registeredFileWriter),
}

func canonicalWriterPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve file logger path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func (r *fileWriterRegistry) acquire(path string) (*fileWriterLease, error) {
	key, err := canonicalWriterPath(path)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	entry := r.writers[key]
	if entry == nil {
		entry = &registeredFileWriter{writer: newBufferedFileWriteSyncer(key)}
		r.writers[key] = entry
	}
	entry.leases++
	r.startSignalWatcherLocked()
	r.mu.Unlock()

	return &fileWriterLease{
		registry: r,
		path:     key,
		writer:   entry.writer,
	}, nil
}

func (r *fileWriterRegistry) startSignalWatcherLocked() {
	if r.signals != nil {
		return
	}
	signals := make(chan os.Signal, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	r.signals = signals
	r.stop = stop
	r.done = done
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		defer close(done)
		for {
			select {
			case <-signals:
				if err := r.flushAndReopenAll(); err != nil {
					logger.Error(fmt.Sprintf("reopen cached log files: %s", err))
				}
			case <-stop:
				return
			}
		}
	}()
}

func (l *fileWriterLease) release() {
	if l == nil || l.registry == nil {
		return
	}
	l.once.Do(func() {
		var closeWriter *bufferedFileWriteSyncer
		var watcherDone chan struct{}
		l.registry.mu.Lock()
		entry := l.registry.writers[l.path]
		if entry != nil {
			entry.leases--
			if entry.leases == 0 {
				delete(l.registry.writers, l.path)
				closeWriter = entry.writer
			}
		}
		if len(l.registry.writers) == 0 {
			if l.registry.signals != nil {
				signal.Stop(l.registry.signals)
			}
			if l.registry.stop != nil {
				close(l.registry.stop)
			}
			watcherDone = l.registry.done
			l.registry.signals = nil
			l.registry.stop = nil
			l.registry.done = nil
		}
		l.registry.mu.Unlock()
		if watcherDone != nil {
			<-watcherDone
		}
		if closeWriter != nil {
			_ = closeWriter.Stop()
		}
	})
}

// FlushAndReopen flushes and releases the cached writer for path. The next
// File Logger write opens the current path, including when the file did not
// exist at the time of this call.
func FlushAndReopen(path string) error {
	key, err := canonicalWriterPath(path)
	if err != nil {
		return err
	}
	return sharedFileWriters.flushAndReopen(key)
}

func (r *fileWriterRegistry) flushAndReopen(path string) error {
	r.mu.Lock()
	entry := r.writers[path]
	r.mu.Unlock()
	if entry == nil {
		return nil
	}
	err := entry.writer.Reopen()
	if err == nil {
		logger.Info("reopen cached log file: " + path)
	}
	return err
}

func (r *fileWriterRegistry) flushAndReopenAll() error {
	r.mu.Lock()
	paths := make([]string, 0, len(r.writers))
	for path := range r.writers {
		paths = append(paths, path)
	}
	r.mu.Unlock()

	var errs []error
	for _, path := range paths {
		if err := r.flushAndReopen(path); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func (r *fileWriterRegistry) has(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writers[path] != nil
}

func (r *fileWriterRegistry) signalWatcherRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signals != nil
}
