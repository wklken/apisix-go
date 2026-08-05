package logger

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	logger       *zap.Logger
	sugarLogger  *zap.SugaredLogger
	runtimeLevel = zap.NewAtomicLevelAt(zap.InfoLevel)
	observers    = observerRegistry{entries: make(map[string]observer)}
)

// Entry is the normalized application log record delivered to observers.
type Entry struct {
	Time    time.Time
	Level   string
	Message string
	Line    string
}

type observer struct {
	generation uint64
	notify     func(Entry)
}

type observerRegistry struct {
	mu         sync.RWMutex
	generation uint64
	entries    map[string]observer
}

func init() {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"} // Replace with your desired log file path
	cfg.Level = runtimeLevel
	logger, _ = cfg.Build()

	sugarLogger = logger.Sugar()
}

func parseAPISIXLogLevel(value string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info", "notice":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn":
		return zapcore.WarnLevel, nil
	case "error", "crit", "alert", "emerg":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("unsupported error_log_level %q", value)
	}
}

func ConfigureLevel(value string) error {
	level, err := parseAPISIXLogLevel(value)
	if err != nil {
		return err
	}
	runtimeLevel.SetLevel(level)
	return nil
}

func DebugEnabled() bool {
	return runtimeLevel.Enabled(zap.DebugLevel)
}

// Use the logger variable to log messages throughout your code
func Info(msg string, fields ...zap.Field) {
	logger.Info(msg, fields...)
	notifyObservers("INFO", msg)
}

func Infof(template string, args ...any) {
	sugarLogger.Infof(template, args)
	notifyObservers("INFO", fmt.Sprintf(template, args...))
}

func Warn(msg string, fields ...zap.Field) {
	logger.Warn(msg, fields...)
	notifyObservers("WARN", msg)
}

func Warnf(template string, args ...any) {
	sugarLogger.Warnf(template, args)
	notifyObservers("WARN", fmt.Sprintf(template, args...))
}

func Error(msg string, fields ...zap.Field) {
	logger.Error(msg, fields...)
	notifyObservers("ERROR", msg)
}

func Errorf(template string, args ...any) {
	sugarLogger.Errorf(template, args...)
	notifyObservers("ERROR", fmt.Sprintf(template, args...))
}

func Debug(msg string, fields ...zap.Field) {
	logger.Debug(msg, fields...)
	notifyObservers("DEBUG", msg)
}

func Debugf(template string, args ...any) {
	sugarLogger.Debugf(template, args...)
	notifyObservers("DEBUG", fmt.Sprintf(template, args...))
}

func Fatal(msg string, fields ...zap.Field) {
	notifyObservers("FATAL", msg)
	logger.Fatal(msg, fields...)
}

func Fatalf(template string, args ...any) {
	notifyObservers("FATAL", fmt.Sprintf(template, args...))
	sugarLogger.Fatalf(template, args...)
}

// ReplaceObserver atomically replaces a named observer. The returned stop
// function removes only this registration, so stopping a retired owner cannot
// remove a newer replacement with the same name.
func ReplaceObserver(name string, notify func(Entry)) func() {
	observers.mu.Lock()
	observers.generation++
	generation := observers.generation
	if notify == nil {
		delete(observers.entries, name)
	} else {
		observers.entries[name] = observer{generation: generation, notify: notify}
	}
	observers.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			observers.mu.Lock()
			if current, ok := observers.entries[name]; ok && current.generation == generation {
				delete(observers.entries, name)
			}
			observers.mu.Unlock()
		})
	}
}

func notifyObservers(level string, message string) {
	now := time.Now()
	entry := Entry{
		Time:    now,
		Level:   level,
		Message: message,
		Line: fmt.Sprintf(
			"%s [%s] %s",
			now.UTC().Format(time.RFC3339Nano),
			strings.ToLower(level),
			message,
		),
	}

	observers.mu.RLock()
	callbacks := make([]func(Entry), 0, len(observers.entries))
	for _, observer := range observers.entries {
		callbacks = append(callbacks, observer.notify)
	}
	observers.mu.RUnlock()

	for _, notify := range callbacks {
		func() {
			defer func() {
				_ = recover()
			}()
			notify(entry)
		}()
	}
}

type DebugLogger struct{}

func (d *DebugLogger) Printf(template string, args ...any) {
	fmt.Printf(template+"\n", args...)
}
