package file_logger

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"time"

	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"go.uber.org/zap/buffer"
)

const (
	fileLoggerQueueCapacity   = 10000
	fileLoggerBatchMaxEntries = 256
	fileLoggerBatchMaxBytes   = 64 * 1024
	// fileLoggerPayloadByteBudget is 1024 maximum-sized batches. It is an
	// internal safety bound for variable data retained by accepted records;
	// the entry limit still bounds the fixed record/channel overhead.
	fileLoggerPayloadByteBudget = 1024 * fileLoggerBatchMaxBytes
	fileLoggerBatchMaxDelay     = 10 * time.Millisecond
	fileLoggerStopTimeout       = 15 * time.Second
)

type fileLoggerProcessorStats struct {
	Pending      int
	PendingBytes int64
	Delivered    int
	Failed       int
}

type fileLoggerSink interface {
	Write([]byte) (int, error)
	Sync() error
}

type fileLogRecordKind uint8

const (
	fileLogSnapshotRecord fileLogRecordKind = iota + 1
	fileLogBarrierRecord
)

type fileLogRecord struct {
	kind          fileLogRecordKind
	snapshot      base.LogSnapshot
	retainedBytes int64
	ack           chan error
}

type fileLoggerProcessor struct {
	sink fileLoggerSink

	records    chan fileLogRecord
	stopSignal chan struct{}
	done       chan struct{}

	admission   sync.RWMutex
	stopped     bool
	stopOnce    sync.Once
	cleanupOnce sync.Once
	lifecycleMu sync.Mutex
	cleanup     func()
	terminal    bool

	observer metrics.LoggerBatchObserver

	snapshotFields func(base.LogSnapshot) map[string]any
	beforeEncode   func()
	encodeFields   func(map[string]any) (*buffer.Buffer, error)

	newTimer    func(time.Duration) *time.Timer
	stopTimeout time.Duration

	pendingMu    sync.Mutex
	pending      int
	pendingBytes int64
	delivered    int
	failed       int

	// payloadByteBudget is kept on the processor so focused tests can use a
	// small budget without changing the production safety bound or a public
	// configuration contract.
	payloadByteBudget int64
}

func newFileLoggerProcessor(
	owner *runtime.TaskOwner,
	sink fileLoggerSink,
) (*fileLoggerProcessor, error) {
	return newFileLoggerProcessorWithTimer(owner, sink, time.NewTimer)
}

func newFileLoggerProcessorWithTimer(
	owner *runtime.TaskOwner,
	sink fileLoggerSink,
	newTimer func(time.Duration) *time.Timer,
) (*fileLoggerProcessor, error) {
	if owner == nil {
		return nil, runtime.ErrTaskOwnerRequired
	}
	processor := &fileLoggerProcessor{
		sink:              sink,
		records:           make(chan fileLogRecord, fileLoggerQueueCapacity),
		stopSignal:        make(chan struct{}),
		done:              make(chan struct{}),
		observer:          metrics.AcquireLoggerBatchObserver("file-logger", "", "", ""),
		snapshotFields:    snapshotDefaultLogFields,
		newTimer:          newTimer,
		stopTimeout:       fileLoggerStopTimeout,
		payloadByteBudget: fileLoggerPayloadByteBudget,
	}
	if err := owner.Go("file-log-writer", processor.run); err != nil {
		processor.observer.Close()
		return nil, err
	}
	return processor, nil
}

func (p *fileLoggerProcessor) pushSnapshot(snapshot base.LogSnapshot) error {
	return p.admit(fileLogRecord{
		kind:     fileLogSnapshotRecord,
		snapshot: snapshot,
	})
}

func (p *fileLoggerProcessor) pushBarrier() (<-chan error, error) {
	ack := make(chan error, 1)
	if err := p.admit(fileLogRecord{kind: fileLogBarrierRecord, ack: ack}); err != nil {
		return nil, err
	}
	return ack, nil
}

func (p *fileLoggerProcessor) admit(record fileLogRecord) error {
	p.admission.RLock()
	defer p.admission.RUnlock()

	if p.stopped {
		p.observer.AddEvent(metrics.LoggerBatchOutcomeStoppedDropped)
		return base.ErrLogQueueUnavailable
	}
	if record.kind == fileLogSnapshotRecord {
		record.retainedBytes = fileLogRecordPayloadBytes(record)
		if !p.reservePending(record.retainedBytes) {
			p.observer.AddEvent(metrics.LoggerBatchOutcomeCapacityDropped)
			return base.ErrLogQueueFull
		}
	}
	select {
	case p.records <- record:
		return nil
	default:
		if record.kind == fileLogSnapshotRecord {
			p.releasePending(1, record.retainedBytes)
		}
		p.observer.AddEvent(metrics.LoggerBatchOutcomeCapacityDropped)
		return base.ErrLogQueueFull
	}
}

func (p *fileLoggerProcessor) run(ctx context.Context) error {
	cancellationDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		defer close(cancellationDone)
		p.sealAdmission()
	})
	defer func() {
		if stopCancellation() {
			close(cancellationDone)
		}
		<-cancellationDone
		p.sealAdmission()
		p.finishTerminal()
	}()

	encoder := newFileLoggerEncoder()

	var batch []byte
	entries := 0
	var batchPayloadBytes int64
	var batchTimer *time.Timer
	var timerC <-chan time.Time
	var deliveryErr error

	stopTimer := func() {
		if batchTimer == nil {
			timerC = nil
			return
		}
		if !batchTimer.Stop() {
			select {
			case <-batchTimer.C:
			default:
			}
		}
		timerC = nil
	}
	armTimer := func() {
		if batchTimer == nil {
			batchTimer = p.newTimer(fileLoggerBatchMaxDelay)
		} else {
			if !batchTimer.Stop() {
				select {
				case <-batchTimer.C:
				default:
				}
			}
			batchTimer.Reset(fileLoggerBatchMaxDelay)
		}
		timerC = batchTimer.C
	}

	flush := func() error {
		if len(batch) == 0 {
			stopTimer()
			return nil
		}
		stopTimer()
		pendingEntries := entries
		pendingPayloadBytes := batchPayloadBytes
		_, err := p.writeBatch(batch)
		p.completeEntries(pendingEntries, pendingPayloadBytes, err == nil)
		batch = batch[:0]
		entries = 0
		batchPayloadBytes = 0
		return err
	}
	sync := func() error {
		if p.sink == nil {
			return nil
		}
		if err := p.sink.Sync(); err != nil {
			p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
			return err
		}
		return nil
	}
	rememberError := func(err error) {
		if err != nil {
			deliveryErr = errors.Join(deliveryErr, err)
		}
	}
	acknowledgeBarrier := func(ack chan error) {
		rememberError(flush())
		rememberError(sync())
		ack <- deliveryErr
		deliveryErr = nil
	}
	handle := func(record fileLogRecord) {
		switch record.kind {
		case fileLogBarrierRecord:
			acknowledgeBarrier(record.ack)
			return
		case fileLogSnapshotRecord:
			if p.beforeEncode != nil {
				p.beforeEncode()
			}
			fields := p.snapshotFields(record.snapshot)
			rememberError(p.appendFields(
				encoder, fields, &batch, &entries, &batchPayloadBytes, record.retainedBytes, armTimer, flush,
			))
		}
	}

	for {
		select {
		case record := <-p.records:
			handle(record)
		case <-timerC:
			rememberError(flush())
		case <-p.stopSignal:
			p.drain(handle, flush, sync, rememberError)
			return nil
		case <-ctx.Done():
			p.sealAdmission()
			p.drain(handle, flush, sync, rememberError)
			return nil
		}
	}
}

func (p *fileLoggerProcessor) drain(
	handle func(fileLogRecord),
	flush func() error,
	syncSink func() error,
	rememberError func(error),
) {
	for {
		select {
		case record := <-p.records:
			handle(record)
		default:
			rememberError(flush())
			rememberError(syncSink())
			return
		}
	}
}

func (p *fileLoggerProcessor) appendFields(
	encoder *fileLoggerEncoder,
	fields map[string]any,
	batch *[]byte,
	entries *int,
	batchPayloadBytes *int64,
	retainedBytes int64,
	armTimer func(),
	flush func() error,
) error {
	var line *buffer.Buffer
	var err error
	if p.encodeFields != nil {
		line, err = p.encodeFields(fields)
	} else {
		line, err = encoder.encode(fields)
	}
	if err != nil {
		p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
		p.completeEntries(1, retainedBytes, false)
		logger.Errorf("file logger encode failed: %s", err)
		return err
	}
	defer line.Free()

	var flushErr error
	lineBytes := line.Bytes()
	if len(*batch) > 0 && len(*batch)+len(lineBytes) > fileLoggerBatchMaxBytes {
		flushErr = errors.Join(flushErr, flush())
	}
	if len(lineBytes) > fileLoggerBatchMaxBytes {
		_, err := p.writeBatch(lineBytes)
		p.completeEntries(1, retainedBytes, err == nil)
		return errors.Join(flushErr, err)
	}
	*batch = append(*batch, lineBytes...)
	(*entries)++
	*batchPayloadBytes += retainedBytes
	if *entries == 1 {
		armTimer()
	}
	if *entries >= fileLoggerBatchMaxEntries || len(*batch) >= fileLoggerBatchMaxBytes {
		flushErr = errors.Join(flushErr, flush())
	}
	return flushErr
}

func (p *fileLoggerProcessor) reservePending(payloadBytes int64) bool {
	p.pendingMu.Lock()
	if payloadBytes < 0 || p.pendingBytes > p.payloadByteBudget-payloadBytes {
		p.pendingMu.Unlock()
		return false
	}
	p.pending++
	p.pendingBytes += payloadBytes
	p.pendingMu.Unlock()
	p.observer.AddPending(1)
	return true
}

func (p *fileLoggerProcessor) releasePending(entries int, payloadBytes int64) {
	if entries <= 0 {
		return
	}
	p.pendingMu.Lock()
	p.pending -= entries
	if p.pending < 0 {
		p.pending = 0
	}
	p.pendingBytes -= payloadBytes
	if p.pendingBytes < 0 {
		p.pendingBytes = 0
	}
	p.pendingMu.Unlock()
	p.observer.AddPending(-entries)
}

func (p *fileLoggerProcessor) completeEntries(entries int, payloadBytes int64, delivered bool) {
	if entries <= 0 {
		return
	}
	p.pendingMu.Lock()
	p.pending -= entries
	if p.pending < 0 {
		p.pending = 0
	}
	p.pendingBytes -= payloadBytes
	if p.pendingBytes < 0 {
		p.pendingBytes = 0
	}
	if delivered {
		p.delivered += entries
	} else {
		p.failed += entries
	}
	p.pendingMu.Unlock()
	p.observer.AddPending(-entries)
}

func (p *fileLoggerProcessor) stats() fileLoggerProcessorStats {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	return fileLoggerProcessorStats{
		Pending:      p.pending,
		PendingBytes: p.pendingBytes,
		Delivered:    p.delivered,
		Failed:       p.failed,
	}
}

const (
	fileLoggerPayloadValueOverhead    int64 = 16
	fileLoggerPayloadMapOverhead      int64 = 48
	fileLoggerPayloadMapEntryOverhead int64 = 32
	fileLoggerPayloadSliceOverhead    int64 = 24
	fileLoggerPayloadScalarBytes      int64 = 8
	fileLoggerPayloadTimeBytes        int64 = 24
	fileLoggerPayloadMax                    = int64(fileLoggerPayloadByteBudget) + 1
)

func fileLogRecordPayloadBytes(record fileLogRecord) int64 {
	switch record.kind {
	case fileLogSnapshotRecord:
		return fileLogSnapshotPayloadBytes(record.snapshot)
	default:
		return 0
	}
}

func fileLogSnapshotPayloadBytes(snapshot base.LogSnapshot) int64 {
	var size int64
	size = addFileLogPayloadBytes(size, fileLogRequestSnapshotPayloadBytes(snapshot.Request))
	size = addFileLogPayloadBytes(size, fileLogResponseSnapshotPayloadBytes(snapshot.Response))
	size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(snapshot.NodeID))
	size = addFileLogPayloadBytes(size, fileLoggerPayloadValueOverhead+fileLoggerPayloadScalarBytes*8)
	size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(string(snapshot.Source)))
	size = addFileLogPayloadBytes(size, fileLoggerPayloadTimeBytes*2)
	return size
}

func fileLogValuePayloadBytes(value any, depth int) int64 {
	if depth > 64 {
		return fileLoggerPayloadMax
	}
	switch typed := value.(type) {
	case nil:
		return fileLoggerPayloadValueOverhead
	case base.LogSnapshot:
		return fileLogSnapshotPayloadBytes(typed)
	case string:
		return fileLogStringPayloadBytes(typed)
	case []byte:
		return addFileLogPayloadBytes(fileLoggerPayloadValueOverhead, int64(len(typed)))
	case bool:
		return fileLoggerPayloadValueOverhead + 1
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64, complex64, complex128:
		return fileLoggerPayloadValueOverhead + fileLoggerPayloadScalarBytes
	case time.Time:
		return fileLoggerPayloadValueOverhead + fileLoggerPayloadTimeBytes
	case time.Duration:
		return fileLoggerPayloadValueOverhead + fileLoggerPayloadScalarBytes
	case http.Header:
		return fileLogHeaderPayloadBytes(typed)
	case url.Values:
		return fileLogHeaderPayloadBytes(http.Header(typed))
	case map[string]any:
		return fileLogAnyMapPayloadBytes(typed, depth)
	case map[string]string:
		return fileLogStringMapPayloadBytes(typed)
	case map[string][]string:
		return fileLogHeaderPayloadBytes(http.Header(typed))
	case []string:
		return fileLogStringSlicePayloadBytes(typed)
	case []any:
		size := fileLoggerPayloadSliceOverhead
		for _, item := range typed {
			size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(item, depth+1))
		}
		return size
	default:
		// CloneSafeValue preserves named scalar types. Account for those by
		// kind; unknown retained values fail closed instead of weakening the
		// byte budget without invoking user code or encoding on the request
		// goroutine.
		reflected := reflect.ValueOf(value)
		switch reflected.Kind() {
		case reflect.String:
			return fileLogStringPayloadBytes(reflected.String())
		case reflect.Bool:
			return fileLoggerPayloadValueOverhead + 1
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
			return fileLoggerPayloadValueOverhead + fileLoggerPayloadScalarBytes
		default:
			return fileLoggerPayloadMax
		}
	}
}

func fileLogRequestSnapshotPayloadBytes(request apisixlog.RequestLogSnapshot) int64 {
	var size int64
	for _, value := range []string{
		request.ID, request.Method, request.URI, request.URL, request.Path,
		request.Host, request.RemoteAddr, request.Scheme, request.Proto,
	} {
		size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(value))
	}
	size = addFileLogPayloadBytes(size, fileLoggerPayloadValueOverhead+fileLoggerPayloadScalarBytes*2)
	size = addFileLogPayloadBytes(size, fileLogHeaderPayloadBytes(request.Header))
	size = addFileLogPayloadBytes(size, fileLogHeaderPayloadBytes(http.Header(request.Query)))
	size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(request.Body, 0))
	size = addFileLogPayloadBytes(size, fileLoggerPayloadValueOverhead+1)
	size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(request.APISIXVars, 0))
	size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(request.RequestVars, 0))
	size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(request.Consumer.Username))
	size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(request.Consumer.GroupID))
	return size
}

func fileLogResponseSnapshotPayloadBytes(response apisixlog.ResponseLogSnapshot) int64 {
	size := fileLogHeaderPayloadBytes(response.Header)
	size = addFileLogPayloadBytes(size, fileLogHeaderPayloadBytes(response.Trailer))
	size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(response.Body, 0))
	size = addFileLogPayloadBytes(size, fileLoggerPayloadValueOverhead+1)
	return size
}

func fileLogAnyMapPayloadBytes(values map[string]any, depth int) int64 {
	size := fileLoggerPayloadMapOverhead
	for key, value := range values {
		size = addFileLogPayloadBytes(size, fileLoggerPayloadMapEntryOverhead+int64(len(key)))
		size = addFileLogPayloadBytes(size, fileLogValuePayloadBytes(value, depth+1))
		if size >= fileLoggerPayloadMax {
			return size
		}
	}
	return size
}

func fileLogStringMapPayloadBytes(values map[string]string) int64 {
	size := fileLoggerPayloadMapOverhead
	for key, value := range values {
		size = addFileLogPayloadBytes(size, fileLoggerPayloadMapEntryOverhead+int64(len(key)))
		size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(value))
		if size >= fileLoggerPayloadMax {
			return size
		}
	}
	return size
}

func fileLogHeaderPayloadBytes(values http.Header) int64 {
	size := fileLoggerPayloadMapOverhead
	for key, entries := range values {
		size = addFileLogPayloadBytes(size, fileLoggerPayloadMapEntryOverhead+int64(len(key)))
		size = addFileLogPayloadBytes(size, fileLoggerPayloadSliceOverhead)
		for _, value := range entries {
			size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(value))
			if size >= fileLoggerPayloadMax {
				return size
			}
		}
	}
	return size
}

func fileLogStringSlicePayloadBytes(values []string) int64 {
	size := fileLoggerPayloadSliceOverhead
	for _, value := range values {
		size = addFileLogPayloadBytes(size, fileLogStringPayloadBytes(value))
		if size >= fileLoggerPayloadMax {
			return size
		}
	}
	return size
}

func fileLogStringPayloadBytes(value string) int64 {
	return addFileLogPayloadBytes(fileLoggerPayloadValueOverhead, int64(len(value)))
}

func addFileLogPayloadBytes(total, delta int64) int64 {
	if delta < 0 || total >= fileLoggerPayloadMax-delta {
		return fileLoggerPayloadMax
	}
	return total + delta
}

func (p *fileLoggerProcessor) writeBatch(data []byte) (int, error) {
	if p.sink == nil {
		err := errors.New("file logger is not initialized")
		p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
		return 0, err
	}
	written, err := p.sink.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
		return written, err
	}
	return written, nil
}

func (p *fileLoggerProcessor) stop() {
	p.stopWithCleanup(nil)
}

func (p *fileLoggerProcessor) stopWithCleanup(cleanup func()) {
	p.registerCleanup(cleanup)
	p.sealAdmission()

	select {
	case <-p.done:
	case <-time.After(p.stopTimeout):
		p.observer.AddEvent(metrics.LoggerBatchOutcomeShutdownTimeout)
	}
}

func (p *fileLoggerProcessor) sealAdmission() {
	p.stopOnce.Do(func() {
		p.admission.Lock()
		p.stopped = true
		close(p.stopSignal)
		p.admission.Unlock()
	})
}

func (p *fileLoggerProcessor) registerCleanup(cleanup func()) {
	if cleanup == nil {
		return
	}
	p.lifecycleMu.Lock()
	if p.cleanup == nil {
		p.cleanup = cleanup
	}
	registered := p.cleanup
	terminal := p.terminal
	p.lifecycleMu.Unlock()
	if terminal {
		p.runCleanup(registered)
	}
}

func (p *fileLoggerProcessor) finishTerminal() {
	p.lifecycleMu.Lock()
	p.terminal = true
	cleanup := p.cleanup
	p.lifecycleMu.Unlock()
	p.observer.Close()
	p.runCleanup(cleanup)
	close(p.done)
}

func (p *fileLoggerProcessor) runCleanup(cleanup func()) {
	if cleanup != nil {
		p.cleanupOnce.Do(cleanup)
	}
}
