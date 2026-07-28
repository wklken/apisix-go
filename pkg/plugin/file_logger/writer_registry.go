package file_logger

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/wklken/apisix-go/pkg/logger"
)

type registeredFileWriter struct {
	writer *appendFileWriteSyncer
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
	writer   *appendFileWriteSyncer
	once     sync.Once
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
		entry = &registeredFileWriter{writer: &appendFileWriteSyncer{path: key}}
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
		var closeWriter *appendFileWriteSyncer
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
			_ = closeWriter.Close()
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
	err := errors.Join(entry.writer.Sync(), entry.writer.Reopen())
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
