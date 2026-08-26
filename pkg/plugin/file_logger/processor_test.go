package file_logger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"go.uber.org/zap/buffer"
)

type recordingFileLoggerSink struct {
	mu         sync.Mutex
	writes     [][]byte
	syncs      int
	writeErr   error
	failWrites int
	short      bool
	entered    chan struct{}
	release    chan struct{}
}

func TestFileLoggerProcessorUsesPluginTaskOwner(t *testing.T) {
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newFileLoggerTaskOwnerForTest(t, registry, "plugin/test/file-logger/route-1")
	processor, err := newFileLoggerProcessor(owner, &recordingFileLoggerSink{})
	if err != nil {
		t.Fatalf("newFileLoggerProcessor() error = %v", err)
	}

	wantOwner := "plugin/test/file-logger/route-1/file-log-writer"
	if active := registry.Active(); !slices.Equal(active, []string{wantOwner}) {
		t.Fatalf("active task owners = %v, want [%s]", active, wantOwner)
	}

	processor.stop()
	stopFileLoggerTaskRegistryForTest(t, registry)
	if active := registry.Active(); len(active) != 0 {
		t.Fatalf("active task owners after stop = %v, want none", active)
	}
}

func TestFileLoggerProcessorPanicSealsAdmission(t *testing.T) {
	failures := make(chan runtime.TaskFailure, 1)
	registry := runtime.NewTaskRegistry(context.Background(), func(failure runtime.TaskFailure) {
		failures <- failure
	})
	owner := newFileLoggerTaskOwnerForTest(t, registry, "plugin/test/file-logger/panic")
	processor, err := newFileLoggerProcessor(owner, &recordingFileLoggerSink{})
	if err != nil {
		t.Fatalf("newFileLoggerProcessor() error = %v", err)
	}
	processor.beforeEncode = func() { panic("file-writer-plugin-panic") }
	if err := processor.pushSnapshot(base.LogSnapshot{}); err != nil {
		t.Fatalf("pushSnapshot() error = %v", err)
	}
	select {
	case failure := <-failures:
		if failure.Owner != "plugin/test/file-logger/panic/file-log-writer" ||
			failure.PanicValue != "file-writer-plugin-panic" {
			t.Fatalf("task failure = %+v, want named file writer panic", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("file writer task panic was not reported")
	}
	if err := processor.pushFields(map[string]any{"late": true}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pushFields() after task panic error = %v, want ErrLogQueueUnavailable", err)
	}
	stopFileLoggerTaskRegistryForTest(t, registry)
}

func TestFileLoggerBlockingWriteIsNamedResidualAndDefersLeaseRelease(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	ownerPrefix := "plugin/test/file-logger/blocked"
	owner := newFileLoggerTaskOwnerForTest(t, tasks, ownerPrefix)
	writers := newFileWriterRegistryForTest(t)
	lease := acquireWriterLeaseForTest(t, writers, filepath.Join(t.TempDir(), "blocked.log"))
	sink := &blockingFileLoggerSink{
		fileLoggerSink: lease.writer,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	processor, err := newFileLoggerProcessor(owner, sink)
	if err != nil {
		lease.release()
		t.Fatalf("newFileLoggerProcessor() error = %v", err)
	}
	if err := processor.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := processor.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("file log writer did not enter blocked Write")
	}

	processor.registerCleanup(lease.release)
	key := lease.path
	if !writers.has(key) {
		t.Fatal("writer lease was released while the owned writer task was blocked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	residuals, stopErr := tasks.Stop(ctx)
	cancel()
	if !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("TaskRegistry.Stop() error = %v, want context deadline exceeded", stopErr)
	}
	wantOwner := ownerPrefix + "/file-log-writer"
	wantResiduals := []runtime.TaskResidual{{Owner: wantOwner}}
	if !slices.Equal(residuals, wantResiduals) {
		t.Fatalf("TaskRegistry.Stop() residuals = %v, want [%s]", residuals, wantOwner)
	}
	var residualErr *runtime.TaskResidualError
	if !errors.As(stopErr, &residualErr) {
		t.Fatalf("TaskRegistry.Stop() error type = %T, want *runtime.TaskResidualError", stopErr)
	}
	if got := residualErr.Residuals(); !slices.Equal(got, wantResiduals) {
		t.Fatalf("TaskResidualError.Residuals() = %v, want %v", got, wantResiduals)
	}
	if err := processor.pushFields(map[string]any{"late": true}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pushFields() after registry cancellation error = %v, want ErrLogQueueUnavailable", err)
	}
	if !writers.has(key) {
		t.Fatal("writer lease was released before the residual task completed")
	}

	close(sink.release)
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	stopFileLoggerTaskRegistryForTest(t, tasks)
	if writers.has(key) {
		t.Fatal("writer lease remains after the owned writer task completed")
	}
}

type blockingFileLoggerSink struct {
	fileLoggerSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingFileLoggerSink) Write(data []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.fileLoggerSink.Write(data)
}

func newFileLoggerTaskOwnerForTest(
	t *testing.T,
	registry *runtime.TaskRegistry,
	prefix string,
) *runtime.TaskOwner {
	t.Helper()
	owner, err := runtime.NewTaskOwner(registry, prefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	return owner
}

func stopFileLoggerTaskRegistryForTest(t *testing.T, registry *runtime.TaskRegistry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if residuals, err := registry.Stop(ctx); err != nil {
		t.Fatalf("TaskRegistry.Stop() residuals = %v, error = %v", residuals, err)
	}
}

func newOwnedFileLoggerProcessorForTest(t *testing.T, sink fileLoggerSink) *fileLoggerProcessor {
	t.Helper()
	processor, err := newFileLoggerProcessor(newFileLoggerTestTaskOwner(t), sink)
	if err != nil {
		t.Fatalf("newFileLoggerProcessor() error = %v", err)
	}
	return processor
}

func newOwnedFileLoggerProcessorWithTimerForTest(
	t *testing.T,
	sink fileLoggerSink,
	newTimer func(time.Duration) *time.Timer,
) *fileLoggerProcessor {
	t.Helper()
	processor, err := newFileLoggerProcessorWithTimer(newFileLoggerTestTaskOwner(t), sink, newTimer)
	if err != nil {
		t.Fatalf("newFileLoggerProcessorWithTimer() error = %v", err)
	}
	return processor
}

func (s *recordingFileLoggerSink) Write(data []byte) (int, error) {
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	if s.writeErr != nil {
		err := s.writeErr
		if s.failWrites > 0 {
			s.failWrites--
			if s.failWrites == 0 {
				s.writeErr = nil
			}
		}
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()
	n := len(data)
	if s.short && n > 0 {
		n--
	}
	s.mu.Lock()
	s.writes = append(s.writes, append([]byte(nil), data[:n]...))
	s.mu.Unlock()
	if n != len(data) {
		return n, nil
	}
	return n, nil
}

func TestFileLoggerProcessorAutomaticFlushErrorsReachNextBarrier(t *testing.T) {
	tests := []struct {
		name          string
		enqueue       func(*testing.T, *fileLoggerProcessor)
		wantDelivered int
		wantFailed    int
	}{
		{
			name: "entry count",
			enqueue: func(t *testing.T, p *fileLoggerProcessor) {
				t.Helper()
				for i := range fileLoggerBatchMaxEntries {
					if err := p.pushFields(map[string]any{"id": i}); err != nil {
						t.Fatalf("pushFields(%d) error = %v", i, err)
					}
				}
			},
			wantFailed: fileLoggerBatchMaxEntries,
		},
		{
			name: "encoded bytes",
			enqueue: func(t *testing.T, p *fileLoggerProcessor) {
				t.Helper()
				for i := range 2 {
					fields := map[string]any{"id": i, "payload": strings.Repeat("x", 40*1024)}
					if err := p.pushFields(fields); err != nil {
						t.Fatalf("pushFields(%d) error = %v", i, err)
					}
				}
			},
			wantDelivered: 1,
			wantFailed:    1,
		},
		{
			name: "oversized entry",
			enqueue: func(t *testing.T, p *fileLoggerProcessor) {
				t.Helper()
				fields := map[string]any{"payload": string(make([]byte, fileLoggerBatchMaxBytes))}
				if err := p.pushFields(fields); err != nil {
					t.Fatalf("pushFields() error = %v", err)
				}
			},
			wantFailed: 1,
		},
		{
			name: "timer",
			enqueue: func(t *testing.T, p *fileLoggerProcessor) {
				t.Helper()
				if err := p.pushFields(map[string]any{"id": 1}); err != nil {
					t.Fatalf("pushFields() error = %v", err)
				}
			},
			wantFailed: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeErr := errors.New("automatic flush failed")
			sink := &recordingFileLoggerSink{
				writeErr: writeErr, failWrites: 1, entered: make(chan struct{}, 1),
			}
			p := newOwnedFileLoggerProcessorForTest(t, sink)
			test.enqueue(t, p)
			select {
			case <-sink.entered:
			case <-time.After(time.Second):
				t.Fatal("automatic boundary did not attempt a Write")
			}
			ack, err := p.pushBarrier()
			if err != nil {
				t.Fatalf("pushBarrier() error = %v", err)
			}
			if err := <-ack; !errors.Is(err, writeErr) {
				t.Fatalf("barrier error = %v, want %v", err, writeErr)
			}
			stats := p.stats()
			if stats.Delivered != test.wantDelivered || stats.Failed != test.wantFailed ||
				stats.Pending != 0 || stats.PendingBytes != 0 {
				t.Fatalf(
					"processor stats = %#v, want delivered=%d failed=%d",
					stats,
					test.wantDelivered,
					test.wantFailed,
				)
			}
			p.stop()
		})
	}
}

func (s *recordingFileLoggerSink) Sync() error {
	s.mu.Lock()
	s.syncs++
	s.mu.Unlock()
	return nil
}

func (s *recordingFileLoggerSink) snapshotWrites() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	writes := make([][]byte, len(s.writes))
	for i := range s.writes {
		writes[i] = append([]byte(nil), s.writes[i]...)
	}
	return writes
}

func TestFileLoggerProcessorFlushesMultipleEntriesInOneWrite(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields(1) error = %v", err)
	}
	if err := p.pushFields(map[string]any{"id": 2}); err != nil {
		t.Fatalf("pushFields(2) error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	p.stop()

	writes := sink.snapshotWrites()
	if len(writes) != 1 {
		t.Fatalf("Write call count = %d, want one", len(writes))
	}
	if got := string(writes[0]); got == "" || got[len(got)-1] != '\n' {
		t.Fatalf("batch = %q, want newline-delimited output", got)
	}
	if count := countNewlines(writes[0]); count != 2 {
		t.Fatalf("batch entry count = %d, want two", count)
	}
	var first, second map[string]any
	lines := splitJSONLines(writes[0])
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("decode first line: %v", err)
	}
	if err := json.Unmarshal(lines[1], &second); err != nil {
		t.Fatalf("decode second line: %v", err)
	}
	if first["id"] != float64(1) || second["id"] != float64(2) {
		t.Fatalf("FIFO ids = %#v, %#v, want 1, 2", first["id"], second["id"])
	}
}

func TestFileLoggerProcessorFlushesByCount(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	p.beforeEncode = func() {}
	for i := range fileLoggerBatchMaxEntries {
		if err := p.pushFields(map[string]any{"id": i}); err != nil {
			t.Fatalf("pushFields(%d) error = %v", i, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshotWrites()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	p.stop()
	if writes := sink.snapshotWrites(); len(writes) == 0 {
		t.Fatal("count boundary did not flush a write")
	}
}

func TestFileLoggerProcessorFlushesByBytesAndOversizedLineAlone(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	if err := p.pushFields(map[string]any{"payload": "small"}); err != nil {
		t.Fatalf("pushFields(small) error = %v", err)
	}
	if err := p.pushFields(map[string]any{"payload": string(make([]byte, fileLoggerBatchMaxBytes))}); err != nil {
		t.Fatalf("pushFields(oversized) error = %v", err)
	}
	p.stop()
	writes := sink.snapshotWrites()
	if len(writes) < 2 {
		t.Fatalf("Write call count = %d, want small and oversized writes", len(writes))
	}
	for i, write := range writes {
		if i < len(writes)-1 && len(write) > fileLoggerBatchMaxBytes {
			t.Fatalf("write %d bytes = %d, want <= batch limit", i, len(write))
		}
	}
}

func TestFileLoggerProcessorFlushesByTimer(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	started := time.Now()
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sink.snapshotWrites()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if len(sink.snapshotWrites()) == 0 {
		t.Fatal("timer did not flush pending entry")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timer flush elapsed = %s, want bounded", elapsed)
	}
	p.stop()
}

func TestFileLoggerProcessorArmsTimerOnlyForNonEmptyBatch(t *testing.T) {
	var timerCreations int
	p := newOwnedFileLoggerProcessorWithTimerForTest(
		t,
		&recordingFileLoggerSink{},
		func(delay time.Duration) *time.Timer {
			timerCreations++
			return time.NewTimer(delay)
		},
	)
	time.Sleep(3 * fileLoggerBatchMaxDelay)
	if timerCreations != 0 {
		t.Fatalf("idle timer creations = %d, want zero", timerCreations)
	}
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	if timerCreations != 1 {
		t.Fatalf("non-empty timer creations = %d, want one", timerCreations)
	}
	p.stop()
}

func TestFileLoggerProcessorPendingWaitsForWriteCompletion(t *testing.T) {
	sink := &recordingFileLoggerSink{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter blocked Write")
	}
	if got := p.pendingCount(); got != 1 {
		t.Fatalf("pending while Write blocked = %d, want one", got)
	}
	close(sink.release)
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	if got := p.pendingCount(); got != 0 {
		t.Fatalf("pending after Write = %d, want zero", got)
	}
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after Write = %d, want zero", got)
	}
	p.stop()
}

func TestFileLoggerProcessorPendingClearsAfterWriteFailure(t *testing.T) {
	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{writeErr: errors.New("write failed")})
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err == nil {
		t.Fatal("barrier error = nil, want write failure")
	}
	if got := p.pendingCount(); got != 0 {
		t.Fatalf("pending after terminal write failure = %d, want zero", got)
	}
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after terminal write failure = %d, want zero", got)
	}
	p.stop()
}

func TestFileLoggerProcessorRejectsPayloadWhenByteBudgetExceeded(t *testing.T) {
	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{})
	p.payloadByteBudget = 64
	if err := p.pushFields(map[string]any{"payload": strings.Repeat("x", 65)}); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("oversized payload error = %v, want ErrLogQueueFull", err)
	}
	stats := p.stats()
	if stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("stats after byte rejection = %#v, want no pending records", stats)
	}
	p.stop()
}

func TestFileLoggerProcessorCountsSnapshotBodiesAgainstByteBudget(t *testing.T) {
	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{})
	p.payloadByteBudget = 64
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Body: []byte(strings.Repeat("x", 65))},
	}
	if err := p.pushSnapshot(snapshot); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("oversized snapshot body error = %v, want ErrLogQueueFull", err)
	}
	if stats := p.stats(); stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("stats after snapshot byte rejection = %#v, want no pending records", stats)
	}
	p.stop()
}

func TestFileLoggerProcessorCountsNamedStringsAgainstByteBudget(t *testing.T) {
	type largeString string

	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{})
	p.payloadByteBudget = 1024
	fields := map[string]any{"payload": largeString(strings.Repeat("x", 2048))}
	if err := p.pushFields(fields); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("oversized named string error = %v, want ErrLogQueueFull", err)
	}
	if stats := p.stats(); stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("stats after named string rejection = %#v, want no pending records", stats)
	}
	p.stop()
}

func TestFileLoggerProcessorReleasesPayloadBytesAndReadmits(t *testing.T) {
	sink := &recordingFileLoggerSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	fields := map[string]any{"payload": strings.Repeat("x", 128)}
	recordBytes := fileLogRecordPayloadBytes(fileLogRecord{kind: fileLogFieldsRecord, fields: fields})
	p.payloadByteBudget = recordBytes
	if err := p.pushFields(fields); err != nil {
		t.Fatalf("first pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin delivering first record")
	}
	if got := p.stats().PendingBytes; got != recordBytes {
		t.Fatalf("pending bytes while first delivery is blocked = %d, want %d", got, recordBytes)
	}
	if err := p.pushFields(fields); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("second pushFields() error = %v, want byte-budget rejection", err)
	}
	if got := p.stats().PendingBytes; got != recordBytes {
		t.Fatalf("pending bytes after rejected record = %d, want %d", got, recordBytes)
	}
	close(sink.release)
	if err := <-ack; err != nil {
		t.Fatalf("first barrier error = %v", err)
	}
	waitForFileLoggerPendingBytes(t, p, 0)
	if err := p.pushFields(fields); err != nil {
		t.Fatalf("readmitted pushFields() error = %v", err)
	}
	ack, err = p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after readmitted delivery = %d, want zero", got)
	}
	p.stop()
}

func TestFileLoggerProcessorEncodeFailureReleasesPayloadBytes(t *testing.T) {
	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{})
	p.encodeFields = func(map[string]any) (*buffer.Buffer, error) {
		return nil, errors.New("file logger test encode failure")
	}
	if err := p.pushFields(map[string]any{"unsupported": "value"}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err == nil {
		t.Fatal("barrier error = nil, want encode failure")
	}
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after encode failure = %d, want zero", got)
	}
	p.stop()
}

func TestFileLoggerProcessorRejectsFullQueueAndStoppedAdmission(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	entered := make(chan struct{})
	release := make(chan struct{})
	p.beforeEncode = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	if err := p.pushSnapshot(base.LogSnapshot{}); err != nil {
		t.Fatalf("pushSnapshot(first) error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach blocked sink")
	}
	for i := 1; i <= fileLoggerQueueCapacity; i++ {
		if err := p.pushSnapshot(base.LogSnapshot{}); err != nil {
			t.Fatalf("pushSnapshot(%d) error = %v before queue full", i, err)
		}
	}
	if err := p.pushSnapshot(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("full queue error = %v, want ErrLogQueueFull", err)
	}
	fullStats := p.stats()
	if fullStats.PendingBytes <= 0 {
		t.Fatalf("pending bytes after filling queue = %d, want positive", fullStats.PendingBytes)
	}
	if err := p.pushSnapshot(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("second full queue error = %v, want ErrLogQueueFull", err)
	}
	if got := p.stats().PendingBytes; got != fullStats.PendingBytes {
		t.Fatalf("pending bytes after channel-full rollback = %d, want %d", got, fullStats.PendingBytes)
	}
	close(release)
	p.stop()
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after stop drain = %d, want zero", got)
	}
	if err := p.pushFields(map[string]any{"id": -2}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("stopped queue error = %v, want ErrLogQueueUnavailable", err)
	}
}

func TestFileLoggerProcessorAdmitsLegacyFieldsAndBarrierAtomically(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	entered := make(chan struct{})
	release := make(chan struct{})
	p.beforeEncode = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	if err := p.pushSnapshot(base.LogSnapshot{}); err != nil {
		t.Fatalf("pushSnapshot() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not block")
	}
	for i := range fileLoggerQueueCapacity - 1 {
		if err := p.pushFields(map[string]any{"id": i}); err != nil {
			t.Fatalf("pushFields(%d) error = %v", i, err)
		}
	}
	ack, err := p.pushFieldsAndBarrier(map[string]any{"legacy": true})
	if err != nil {
		t.Fatalf("pushFieldsAndBarrier() with final queue slot error = %v", err)
	}
	if err := p.pushFields(map[string]any{"overflow": true}); !errors.Is(err, base.ErrLogQueueFull) {
		t.Fatalf("overflow error = %v, want ErrLogQueueFull", err)
	}
	close(release)
	select {
	case err := <-ack:
		if err != nil {
			t.Fatalf("combined barrier error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("combined barrier did not acknowledge")
	}
	if got := p.stats().PendingBytes; got != 0 {
		t.Fatalf("pending bytes after combined barrier = %d, want zero", got)
	}
	p.stop()
}

func TestFileLoggerRunLogPhaseQueuesSnapshotBeforeFieldConstruction(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	processor := newOwnedFileLoggerProcessorForTest(t, sink)
	plugin := &Plugin{
		config: Config{
			LogFormat: map[string]any{"uri": "$uri"},
		},
		logFormat: map[string]any{"uri": "$uri"},
		processor: processor,
	}
	processor.snapshotFields = plugin.buildSnapshotFields
	entered := make(chan struct{})
	release := make(chan struct{})
	processor.beforeEncode = func() {
		close(entered)
		<-release
	}

	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			URI: "/before",
		},
	}
	result := make(chan error, 1)
	go func() {
		result <- plugin.RunLogPhase(snapshot)
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("RunLogPhase() error = %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("RunLogPhase() blocked while worker was blocked")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not block before field construction")
	}
	close(release)
	ack, err := processor.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
	plugin.Stop()

	lines := splitJSONLines(flattenWrites(sink.snapshotWrites()))
	if len(lines) != 1 {
		t.Fatalf("logged lines = %d, want one", len(lines))
	}
	var decoded map[string]any
	if err := json.Unmarshal(lines[0], &decoded); err != nil {
		t.Fatalf("decode logged line: %v", err)
	}
	if decoded["uri"] != "/before" {
		t.Fatalf("queued URI = %#v, want /before", decoded["uri"])
	}
}

func TestFileLoggerProcessorConcurrentPushStop(t *testing.T) {
	p := newOwnedFileLoggerProcessorForTest(t, &recordingFileLoggerSink{})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 100 {
				_ = p.pushFields(map[string]any{"id": i*100 + j})
			}
		})
	}
	wg.Go(func() {
		p.stop()
	})
	wg.Wait()
	p.stop()
}

func TestFileLoggerProcessorBarrierFlushesAndSyncs(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	select {
	case err := <-ack:
		if err != nil {
			t.Fatalf("barrier error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("barrier did not acknowledge")
	}
	sink.mu.Lock()
	syncs := sink.syncs
	sink.mu.Unlock()
	if syncs == 0 {
		t.Fatal("barrier did not sync sink")
	}
	p.stop()
}

func TestFileLoggerProcessorReportsWriteAndShortWriteErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	for name, sink := range map[string]*recordingFileLoggerSink{
		"write error": {writeErr: writeErr},
		"short write": {short: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := newOwnedFileLoggerProcessorForTest(t, sink)
			if _, err := p.writeBatch([]byte("line\n")); !errors.Is(err, writeErr) && name == "write error" {
				t.Fatalf("writeBatch() error = %v, want %v", err, writeErr)
			}
			if name == "short write" {
				if _, err := p.writeBatch([]byte("line\n")); !errors.Is(err, io.ErrShortWrite) {
					t.Fatalf("writeBatch() error = %v, want %v", err, io.ErrShortWrite)
				}
			}
			p.stop()
		})
	}
}

func TestFileLoggerProcessorStopIsIdempotentAndDrains(t *testing.T) {
	sink := &recordingFileLoggerSink{}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	for i := range 3 {
		if err := p.pushFields(map[string]any{"id": i}); err != nil {
			t.Fatalf("pushFields(%d) error = %v", i, err)
		}
	}
	p.stop()
	p.stop()
	if got := countNewlines(flattenWrites(sink.snapshotWrites())); got != 3 {
		t.Fatalf("drained entries = %d, want three", got)
	}
}

func TestFileLoggerProcessorTimeoutRetainsCleanupUntilWorkerFinishes(t *testing.T) {
	sink := &recordingFileLoggerSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := newOwnedFileLoggerProcessorForTest(t, sink)
	p.stopTimeout = 10 * time.Millisecond
	if err := p.pushFields(map[string]any{"id": 1}); err != nil {
		t.Fatalf("pushFields() error = %v", err)
	}
	ack, err := p.pushBarrier()
	if err != nil {
		t.Fatalf("pushBarrier() error = %v", err)
	}
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter blocked Write")
	}
	cleaned := make(chan struct{})
	p.stopWithCleanup(func() { close(cleaned) })
	select {
	case <-cleaned:
		t.Fatal("cleanup ran while worker still owned accepted records")
	default:
	}
	close(sink.release)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not run after worker finished")
	}
	if err := <-ack; err != nil {
		t.Fatalf("barrier error = %v", err)
	}
}

func countNewlines(data []byte) int {
	count := 0
	for _, byteValue := range data {
		if byteValue == '\n' {
			count++
		}
	}
	return count
}

func flattenWrites(writes [][]byte) []byte {
	var flattened []byte
	for _, write := range writes {
		flattened = append(flattened, write...)
	}
	return flattened
}

func splitJSONLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for index, value := range data {
		if value != '\n' {
			continue
		}
		lines = append(lines, append([]byte(nil), data[start:index]...))
		start = index + 1
	}
	return lines
}

func waitForFileLoggerPendingBytes(t *testing.T, p *fileLoggerProcessor, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := p.stats().PendingBytes; got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending bytes = %d, want %d", p.stats().PendingBytes, want)
}
