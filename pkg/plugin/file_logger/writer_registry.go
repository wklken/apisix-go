package file_logger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/runtime"
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
	mu            sync.Mutex
	writers       map[string]*registeredFileWriter
	signals       chan os.Signal
	watcherEpoch  *runtime.TaskRegistry
	epochStopping chan struct{}

	notifySignal func(chan<- os.Signal, ...os.Signal)
	stopSignal   func(chan<- os.Signal)
	reopenAll    func() error
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

	for {
		r.mu.Lock()
		if stopping := r.epochStopping; stopping != nil {
			r.mu.Unlock()
			<-stopping
			continue
		}
		entry := r.writers[key]
		if entry == nil {
			entry = &registeredFileWriter{writer: newBufferedFileWriteSyncer(key)}
			r.writers[key] = entry
		}
		entry.leases++
		if err := r.startSignalWatcherLocked(); err != nil {
			entry.leases--
			stopWriter := entry.leases == 0
			if stopWriter {
				delete(r.writers, key)
			}
			r.mu.Unlock()
			if stopWriter {
				_ = entry.writer.Stop()
			}
			return nil, err
		}
		r.mu.Unlock()

		return &fileWriterLease{
			registry: r,
			path:     key,
			writer:   entry.writer,
		}, nil
	}
}

func (r *fileWriterRegistry) startSignalWatcherLocked() error {
	if r.watcherEpoch != nil {
		return nil
	}
	signals := make(chan os.Signal, 1)
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "core/file-writer-registry", runtime.TaskCore)
	if err != nil {
		return err
	}
	notifySignal := r.notifySignal
	if notifySignal == nil {
		notifySignal = signal.Notify
	}
	stopSignal := r.stopSignal
	if stopSignal == nil {
		stopSignal = signal.Stop
	}
	notifySignal(signals, syscall.SIGUSR1)
	r.signals = signals
	r.watcherEpoch = tasks
	if err := owner.Go("signal-watch", func(ctx context.Context) error {
		return r.watchSignals(ctx, signals)
	}); err != nil {
		stopSignal(signals)
		r.signals = nil
		r.watcherEpoch = nil
		_, _ = tasks.Stop(context.Background())
		return err
	}
	return nil
}

func (r *fileWriterRegistry) watchSignals(ctx context.Context, signals <-chan os.Signal) error {
	for {
		select {
		case <-signals:
			r.mu.Lock()
			reopenAll := r.reopenAll
			r.mu.Unlock()
			if reopenAll == nil {
				reopenAll = r.flushAndReopenAll
			}
			if err := reopenAll(); err != nil {
				logger.Error(fmt.Sprintf("reopen cached log files: %s", err))
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (l *fileWriterLease) release() {
	if l == nil || l.registry == nil {
		return
	}
	l.once.Do(func() {
		var closeWriter *bufferedFileWriteSyncer
		var watcherEpoch *runtime.TaskRegistry
		var epochStopped chan struct{}
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
			if l.registry.watcherEpoch != nil {
				stopSignal := l.registry.stopSignal
				if stopSignal == nil {
					stopSignal = signal.Stop
				}
				stopSignal(l.registry.signals)
				watcherEpoch = l.registry.watcherEpoch
				epochStopped = make(chan struct{})
				l.registry.epochStopping = epochStopped
			}
			l.registry.signals = nil
			l.registry.watcherEpoch = nil
		}
		l.registry.mu.Unlock()
		if watcherEpoch != nil {
			_, _ = watcherEpoch.Stop(context.Background())
		}
		if closeWriter != nil {
			_ = closeWriter.Stop()
		}
		if epochStopped != nil {
			l.registry.mu.Lock()
			if l.registry.epochStopping == epochStopped {
				l.registry.epochStopping = nil
				close(epochStopped)
			}
			l.registry.mu.Unlock()
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
	return r.watcherEpoch != nil
}
