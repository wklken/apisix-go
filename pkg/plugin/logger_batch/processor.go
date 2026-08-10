package logger_batch

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

const (
	DefaultBatchMaxSize    = 1000
	DefaultMaxRetryCount   = 0
	DefaultRetryDelay      = time.Second
	DefaultBufferDuration  = time.Minute
	DefaultInactiveTimeout = 5 * time.Second

	DefaultMaxPendingEntries       = 10000
	DefaultMaxConcurrentDeliveries = 1
	DefaultDeliveryTimeout         = 10 * time.Second
	DefaultShutdownTimeout         = 15 * time.Second
	maxConcurrentDeliveries        = 8
)

type DeliveryFunc func(entries []map[string]any, batchMaxSize int) (firstFail int, err error)

// ContextDeliveryFunc must stop transport work and return after ctx is done.
// Sink resources remain owned until the callback has actually returned.
type ContextDeliveryFunc func(ctx context.Context, entries []map[string]any, batchMaxSize int) (firstFail int, err error)

type Config struct {
	Name              string
	PluginID          string
	BatchMaxSize      int
	MaxRetryCount     int
	RetryDelay        time.Duration
	RetryDelaySet     bool
	BufferDuration    time.Duration
	InactiveTimeout   time.Duration
	MaxPendingEntries int

	MaxConcurrentDeliveries int
	DeliveryTimeout         time.Duration
	ShutdownTimeout         time.Duration

	RouteID    string
	ServerAddr string
}

type workBatch struct {
	entries  []map[string]any
	terminal bool
}

type Processor struct {
	config  Config
	deliver ContextDeliveryFunc

	deliveryCtx context.Context
	cancel      context.CancelFunc
	observer    metrics.LoggerBatchObserver

	mu          sync.Mutex
	cond        *sync.Cond
	wg          sync.WaitGroup
	timer       *time.Timer
	timerGen    uint64
	workerCount int
	ready       []*workBatch
	active      map[*workBatch]struct{}

	stopped          bool
	shutdownOnce     sync.Once
	waitOnce         sync.Once
	shutdownDone     chan struct{}
	workersDone      chan struct{}
	cleanupOnce      sync.Once
	shutdownFinished bool
	shutdownAborted  bool
	shutdownErr      error

	buffer      []map[string]any
	firstEntry  time.Time
	lastEntry   time.Time
	pending     int
	processing  int
	dropped     int
	delivered   int
	failedDrops int
}

func New(config Config, deliver DeliveryFunc) *Processor {
	return NewWithContext(config, func(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
		if deliver == nil {
			return 0, errors.New("logger batch delivery function is nil")
		}
		return deliver(entries, batchMaxSize)
	})
}

func NewWithContext(config Config, deliver ContextDeliveryFunc) *Processor {
	config.applyDefaults()
	deliveryCtx, cancel := context.WithCancel(context.Background())
	p := &Processor{
		config:      config,
		deliver:     deliver,
		deliveryCtx: deliveryCtx,
		cancel:      cancel,
		observer: metrics.AcquireLoggerBatchObserver(
			config.PluginID,
			config.Name,
			config.RouteID,
			config.ServerAddr,
		),
		workerCount:  config.MaxConcurrentDeliveries,
		active:       make(map[*workBatch]struct{}),
		buffer:       make([]map[string]any, 0, config.BatchMaxSize),
		shutdownDone: make(chan struct{}),
		workersDone:  make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)
	p.wg.Add(p.workerCount)
	for i := 0; i < p.workerCount; i++ {
		go p.worker()
	}
	return p
}

func (p *Processor) Push(entry map[string]any) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopped {
		p.dropped++
		p.observer.AddEvent(metrics.LoggerBatchOutcomeStoppedDropped)
		return false
	}
	if p.pending >= p.config.MaxPendingEntries {
		p.dropped++
		p.observer.AddEvent(metrics.LoggerBatchOutcomeCapacityDropped)
		logger.Errorf(
			"max pending entries limit exceeded for logger batch processor [%s], pending [%d], max_pending_entries [%d]",
			p.config.Name,
			p.pending,
			p.config.MaxPendingEntries,
		)
		return false
	}

	now := time.Now()
	if len(p.buffer) == 0 {
		p.firstEntry = now
	}
	p.lastEntry = now
	p.buffer = append(p.buffer, entry)
	p.pending++
	p.observer.AddPending(1)
	p.setBufferedMetricLocked()

	if len(p.buffer) >= p.config.BatchMaxSize {
		p.flushLocked()
	} else {
		p.scheduleTimerLocked()
	}
	return true
}

func (p *Processor) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
}

func (p *Processor) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), p.config.ShutdownTimeout)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		logger.Errorf("logger batch processor [%s] shutdown: %s", p.config.Name, err)
	}
}

// StopWithCleanup keeps delivery-owned resources alive until every callback
// has returned. Stop remains bounded; if a callback ignores cancellation, the
// cleanup runs asynchronously after that callback eventually exits.
func (p *Processor) StopWithCleanup(cleanup func()) {
	p.Stop()
	if cleanup == nil {
		return
	}
	p.cleanupOnce.Do(func() {
		select {
		case <-p.workersDone:
			cleanup()
		default:
			go func() {
				<-p.workersDone
				cleanup()
			}()
		}
	})
}

func (p *Processor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.startShutdown()
	if err, done := p.shutdownResult(); done {
		return err
	}

	select {
	case <-p.shutdownDone:
		err, _ := p.shutdownResult()
		return err
	case <-ctx.Done():
		p.abort(ctx.Err())
		err, _ := p.shutdownResult()
		return err
	}
}

func (p *Processor) shutdownResult() (error, bool) {
	select {
	case <-p.shutdownDone:
		p.mu.Lock()
		err := p.shutdownErr
		p.mu.Unlock()
		return err, true
	default:
		return nil, false
	}
}

func (p *Processor) startShutdown() {
	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.stopped = true
		p.flushLocked()
		p.stopTimerLocked()
		p.cond.Broadcast()
		p.mu.Unlock()

		p.waitOnce.Do(func() {
			go func() {
				p.wg.Wait()
				close(p.workersDone)
				p.finishShutdown(nil)
			}()
		})
	})
}

func (p *Processor) abort(reason error) {
	p.mu.Lock()
	if p.shutdownFinished {
		p.mu.Unlock()
		return
	}
	if !p.shutdownAborted {
		p.shutdownAborted = true
		p.shutdownErr = reason
		p.cancel()
		dropped := p.dropUndispatchedLocked()
		p.cond.Broadcast()
		if dropped > 0 {
			p.observer.AddEvent(metrics.LoggerBatchOutcomeStoppedDropped)
		}
		p.observer.AddEvent(metrics.LoggerBatchOutcomeShutdownTimeout)
	}
	p.mu.Unlock()
	// An uncooperative legacy callback cannot be interrupted. Close the
	// caller-visible lifecycle after accounting so Stop remains bounded. Sink
	// resources stay owned by workersDone until the callback eventually returns.
	p.finishShutdown(reason)
}

func (p *Processor) finishShutdown(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdownFinished {
		return
	}
	p.shutdownFinished = true
	if p.shutdownErr == nil {
		p.shutdownErr = err
	}
	p.observer.Close()
	close(p.shutdownDone)
}

func (p *Processor) worker() {
	defer p.wg.Done()
	for {
		batch := p.nextBatch()
		if batch == nil {
			return
		}
		p.process(batch)
	}
}

func (p *Processor) nextBatch() *workBatch {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.ready) == 0 && !p.stopped {
		p.cond.Wait()
	}
	if len(p.ready) == 0 {
		return nil
	}
	batch := p.ready[0]
	p.ready[0] = nil
	p.ready = p.ready[1:]
	p.active[batch] = struct{}{}
	return batch
}

func (p *Processor) process(batch *workBatch) {
	for attempt := 0; ; attempt++ {
		p.mu.Lock()
		if batch.terminal || len(batch.entries) == 0 {
			p.mu.Unlock()
			return
		}
		entries := append([]map[string]any(nil), batch.entries...)
		p.mu.Unlock()

		attemptCtx, cancel := context.WithTimeout(p.deliveryCtx, p.config.DeliveryTimeout)
		var firstFail int
		var err error
		if p.deliver == nil {
			err = errors.New("logger batch delivery function is nil")
		} else {
			firstFail, err = p.deliver(attemptCtx, entries, p.config.BatchMaxSize)
		}
		attemptErr := attemptCtx.Err()
		cancel()

		p.mu.Lock()
		if batch.terminal {
			p.mu.Unlock()
			return
		}
		if err == nil {
			p.finishBatchLocked(batch, len(entries), true)
			p.mu.Unlock()
			return
		}

		if firstFail > 1 && firstFail <= len(entries) {
			p.completePrefixLocked(batch, firstFail-1)
			entries = append([]map[string]any(nil), entries[firstFail-1:]...)
			batch.entries = entries
		}
		if len(entries) == 0 {
			p.mu.Unlock()
			return
		}
		if attempt >= p.config.MaxRetryCount {
			timedOut := errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, os.ErrDeadlineExceeded) ||
				errors.Is(attemptErr, context.DeadlineExceeded)
			p.finishBatchLocked(batch, len(entries), false)
			if timedOut {
				p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryTimeout)
			} else {
				p.observer.AddEvent(metrics.LoggerBatchOutcomeDeliveryFailed)
			}
			logger.Errorf(
				"logger batch processor [%s] exceeded max_retry_count [%d], dropping %d entries: %s",
				p.config.Name,
				p.config.MaxRetryCount,
				len(entries),
				err,
			)
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		if !p.waitRetry() {
			return
		}
	}
}

func (p *Processor) waitRetry() bool {
	if p.config.RetryDelay <= 0 {
		select {
		case <-p.deliveryCtx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(p.config.RetryDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-p.deliveryCtx.Done():
		return false
	}
}

func (p *Processor) finishBatchLocked(batch *workBatch, count int, delivered bool) {
	if batch.terminal {
		return
	}
	count = minInt(count, len(batch.entries))
	if count <= 0 {
		batch.terminal = true
		delete(p.active, batch)
		return
	}
	p.decrementPendingLocked(count)
	p.processing -= count
	if p.processing < 0 {
		p.processing = 0
	}
	if delivered {
		p.delivered += count
	} else {
		p.failedDrops += count
	}
	batch.entries = batch.entries[count:]
	if len(batch.entries) == 0 {
		batch.terminal = true
		delete(p.active, batch)
	}
}

func (p *Processor) completePrefixLocked(batch *workBatch, count int) {
	count = minInt(count, len(batch.entries))
	if count <= 0 {
		return
	}
	p.decrementPendingLocked(count)
	p.processing -= count
	if p.processing < 0 {
		p.processing = 0
	}
	p.delivered += count
	batch.entries = batch.entries[count:]
}

func (p *Processor) decrementPendingLocked(count int) {
	if count > p.pending {
		count = p.pending
	}
	p.pending -= count
	if count > 0 {
		p.observer.AddPending(-count)
	}
}

func (p *Processor) dropUndispatchedLocked() int {
	dropped := len(p.buffer)
	p.buffer = p.buffer[:0]
	p.firstEntry = time.Time{}
	p.lastEntry = time.Time{}
	p.setBufferedMetricLocked()
	for _, batch := range p.ready {
		if batch != nil && !batch.terminal {
			batch.terminal = true
			dropped += len(batch.entries)
			batch.entries = nil
		}
	}
	p.ready = nil
	for batch := range p.active {
		if !batch.terminal {
			batch.terminal = true
			dropped += len(batch.entries)
			batch.entries = nil
		}
		delete(p.active, batch)
	}
	if dropped > 0 {
		p.decrementPendingLocked(dropped)
		p.processing = 0
		p.failedDrops += dropped
	}
	return dropped
}

func (p *Processor) scheduleTimerLocked() {
	if p.stopped || len(p.buffer) == 0 {
		return
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timerGen++
	generation := p.timerGen
	deadline := p.firstEntry.Add(p.config.BufferDuration)
	inactiveDeadline := p.lastEntry.Add(p.config.InactiveTimeout)
	if inactiveDeadline.Before(deadline) {
		deadline = inactiveDeadline
	}
	delay := time.Until(deadline)
	delay = max(delay, 0)
	p.timer = time.AfterFunc(delay, func() { p.onTimer(generation) })
}

func (p *Processor) onTimer(generation uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if generation != p.timerGen || p.timer == nil {
		return
	}
	p.timer = nil
	if p.stopped || len(p.buffer) == 0 {
		return
	}
	deadline := p.firstEntry.Add(p.config.BufferDuration)
	inactiveDeadline := p.lastEntry.Add(p.config.InactiveTimeout)
	if inactiveDeadline.Before(deadline) {
		deadline = inactiveDeadline
	}
	if time.Now().Before(deadline) {
		p.scheduleTimerLocked()
		return
	}
	p.flushLocked()
}

func (p *Processor) flushLocked() {
	if len(p.buffer) == 0 {
		return
	}
	batch := &workBatch{entries: append([]map[string]any(nil), p.buffer...)}
	p.buffer = p.buffer[:0]
	p.firstEntry = time.Time{}
	p.lastEntry = time.Time{}
	p.setBufferedMetricLocked()
	p.stopTimerLocked()
	p.processing += len(batch.entries)
	p.ready = append(p.ready, batch)
	p.cond.Broadcast()
}

func (p *Processor) setBufferedMetricLocked() {
	if p.config.Name != "" && p.config.RouteID != "" && p.config.ServerAddr != "" {
		metrics.SetBatchProcessEntries(p.config.Name, p.config.RouteID, p.config.ServerAddr, len(p.buffer))
	}
	p.observer.SetBuffered(len(p.buffer))
}

func (p *Processor) stopTimerLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.timerGen++
}

func (c *Config) applyDefaults() {
	if c.Name == "" {
		c.Name = "log buffer"
	}
	if c.BatchMaxSize <= 0 {
		c.BatchMaxSize = DefaultBatchMaxSize
	}
	if c.RetryDelay < 0 || c.RetryDelay == 0 && !c.RetryDelaySet {
		c.RetryDelay = DefaultRetryDelay
	}
	if c.BufferDuration <= 0 {
		c.BufferDuration = DefaultBufferDuration
	}
	if c.InactiveTimeout <= 0 {
		c.InactiveTimeout = DefaultInactiveTimeout
	}
	if c.MaxRetryCount < 0 {
		c.MaxRetryCount = DefaultMaxRetryCount
	}
	if c.MaxPendingEntries <= 0 {
		c.MaxPendingEntries = DefaultMaxPendingEntries
	}
	if c.MaxConcurrentDeliveries <= 0 {
		c.MaxConcurrentDeliveries = DefaultMaxConcurrentDeliveries
	}
	if c.MaxConcurrentDeliveries > maxConcurrentDeliveries {
		c.MaxConcurrentDeliveries = maxConcurrentDeliveries
	}
	if c.DeliveryTimeout <= 0 {
		c.DeliveryTimeout = DefaultDeliveryTimeout
	}
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type Stats struct {
	Pending     int
	Processing  int
	Buffered    int
	Dropped     int
	Delivered   int
	FailedDrops int
}

func (p *Processor) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Pending:     p.pending,
		Processing:  p.processing,
		Buffered:    len(p.buffer),
		Dropped:     p.dropped,
		Delivered:   p.delivered,
		FailedDrops: p.failedDrops,
	}
}
