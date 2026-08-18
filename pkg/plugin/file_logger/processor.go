package file_logger

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

const (
	fileLoggerQueueCapacity   = 10000
	fileLoggerBatchMaxEntries = 256
	fileLoggerBatchMaxBytes   = 64 * 1024
	fileLoggerBatchMaxDelay   = 10 * time.Millisecond
	fileLoggerStopTimeout     = 15 * time.Second
)

type fileLoggerProcessorStats struct {
	Pending   int
	Delivered int
	Failed    int
}

type fileLoggerSink interface {
	Write([]byte) (int, error)
	Sync() error
}

type fileLogRecordKind uint8

const (
	fileLogSnapshotRecord fileLogRecordKind = iota + 1
	fileLogFieldsRecord
	fileLogBarrierRecord
	fileLogFieldsBarrierRecord
)

type fileLogRecord struct {
	kind     fileLogRecordKind
	snapshot base.LogSnapshot
	fields   map[string]any
	ack      chan error
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

	observer metrics.LoggerBatchObserver

	snapshotFields func(base.LogSnapshot) map[string]any
	beforeEncode   func()

	newTimer    func(time.Duration) *time.Timer
	stopTimeout time.Duration

	pendingMu sync.Mutex
	pending   int
	delivered int
	failed    int
}

func newFileLoggerProcessor(sink fileLoggerSink) *fileLoggerProcessor {
	return newFileLoggerProcessorWithTimer(sink, time.NewTimer)
}

func newFileLoggerProcessorWithTimer(
	sink fileLoggerSink,
	newTimer func(time.Duration) *time.Timer,
) *fileLoggerProcessor {
	processor := &fileLoggerProcessor{
		sink:           sink,
		records:        make(chan fileLogRecord, fileLoggerQueueCapacity),
		stopSignal:     make(chan struct{}),
		done:           make(chan struct{}),
		observer:       metrics.AcquireLoggerBatchObserver("file-logger", "", "", ""),
		snapshotFields: snapshotDefaultLogFields,
		newTimer:       newTimer,
		stopTimeout:    fileLoggerStopTimeout,
	}
	go processor.run()
	return processor
}

func (p *fileLoggerProcessor) pushSnapshot(snapshot base.LogSnapshot) error {
	return p.admit(fileLogRecord{
		kind:     fileLogSnapshotRecord,
		snapshot: snapshot,
	})
}

func (p *fileLoggerProcessor) pushFields(fields map[string]any) error {
	return p.admit(fileLogRecord{
		kind:   fileLogFieldsRecord,
		fields: fields,
	})
}

func (p *fileLoggerProcessor) pushBarrier() (<-chan error, error) {
	ack := make(chan error, 1)
	if err := p.admit(fileLogRecord{kind: fileLogBarrierRecord, ack: ack}); err != nil {
		return nil, err
	}
	return ack, nil
}

func (p *fileLoggerProcessor) pushFieldsAndBarrier(fields map[string]any) (<-chan error, error) {
	ack := make(chan error, 1)
	if err := p.admit(fileLogRecord{kind: fileLogFieldsBarrierRecord, fields: fields, ack: ack}); err != nil {
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
	if record.kind != fileLogBarrierRecord {
		p.addPending(1)
	}
	select {
	case p.records <- record:
		return nil
	default:
		if record.kind != fileLogBarrierRecord {
			p.addPending(-1)
		}
		p.observer.AddEvent(metrics.LoggerBatchOutcomeCapacityDropped)
		return base.ErrLogQueueFull
	}
}

func (p *fileLoggerProcessor) run() {
	defer close(p.done)

	encoder := newFileLoggerEncoder()

	var batch []byte
	entries := 0
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
		_, err := p.writeBatch(batch)
		p.completeEntries(pendingEntries, err == nil)
		batch = batch[:0]
		entries = 0
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
			rememberError(p.appendFields(encoder, fields, &batch, &entries, armTimer, flush))
		case fileLogFieldsRecord:
			rememberError(p.appendFields(encoder, record.fields, &batch, &entries, armTimer, flush))
		case fileLogFieldsBarrierRecord:
			rememberError(p.appendFields(encoder, record.fields, &batch, &entries, armTimer, flush))
			acknowledgeBarrier(record.ack)
		}
	}

	for {
		select {
		case record := <-p.records:
			handle(record)
		case <-timerC:
			rememberError(flush())
		case <-p.stopSignal:
			for {
				select {
				case record := <-p.records:
					handle(record)
				default:
					rememberError(flush())
					rememberError(sync())
					return
				}
			}
		}
	}
}

func (p *fileLoggerProcessor) appendFields(
	encoder *fileLoggerEncoder,
	fields map[string]any,
	batch *[]byte,
	entries *int,
	armTimer func(),
	flush func() error,
) error {
	line, err := encoder.encode(fields)
	if err != nil {
		p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
		p.completeEntries(1, false)
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
		p.completeEntries(1, err == nil)
		return errors.Join(flushErr, err)
	}
	*batch = append(*batch, lineBytes...)
	(*entries)++
	if *entries == 1 {
		armTimer()
	}
	if *entries >= fileLoggerBatchMaxEntries || len(*batch) >= fileLoggerBatchMaxBytes {
		flushErr = errors.Join(flushErr, flush())
	}
	return flushErr
}

func (p *fileLoggerProcessor) addPending(delta int) {
	p.pendingMu.Lock()
	p.pending += delta
	if p.pending < 0 {
		p.pending = 0
	}
	p.pendingMu.Unlock()
	p.observer.AddPending(delta)
}

func (p *fileLoggerProcessor) pendingCount() int {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	return p.pending
}

func (p *fileLoggerProcessor) completeEntries(entries int, delivered bool) {
	if entries <= 0 {
		return
	}
	p.pendingMu.Lock()
	p.pending -= entries
	if p.pending < 0 {
		p.pending = 0
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
	return fileLoggerProcessorStats{Pending: p.pending, Delivered: p.delivered, Failed: p.failed}
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
	p.stopOnce.Do(func() {
		p.admission.Lock()
		p.stopped = true
		close(p.stopSignal)
		p.admission.Unlock()
	})

	finish := func() {
		p.cleanupOnce.Do(func() {
			p.observer.Close()
			if cleanup != nil {
				cleanup()
			}
		})
	}
	select {
	case <-p.done:
		finish()
	case <-time.After(p.stopTimeout):
		p.observer.AddEvent(metrics.LoggerBatchOutcomeShutdownTimeout)
		go func() {
			<-p.done
			finish()
		}()
	}
}
