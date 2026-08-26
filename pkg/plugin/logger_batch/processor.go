package logger_batch

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/runtime"
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
	Tasks             *runtime.TaskOwner
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
	timerGen    uint64
	workerCount int
	workersLeft int
	ready       []*workBatch
	active      map[*workBatch]struct{}

	stopped         bool
	stopRequested   bool
	flushRequested  bool
	admissionFailed bool
	terminal        bool
	shutdownErr     error
	cleanup         func()

	wakeScheduler  chan struct{}
	admissionDone  chan struct{}
	schedulerReady chan struct{}
	schedulerDone  chan struct{}
	workersDone    chan struct{}
	shutdownDone   chan struct{}
	admissionOnce  sync.Once
	schedulerOnce  sync.Once
	workersOnce    sync.Once
	shutdownOnce   sync.Once
	cleanupOnce    sync.Once

	buffer      []map[string]any
	firstEntry  time.Time
	lastEntry   time.Time
	pending     int
	processing  int
	dropped     int
	delivered   int
	failedDrops int
}

func New(config Config, deliver DeliveryFunc) (*Processor, error) {
	return NewWithContext(config, func(ctx context.Context, entries []map[string]any, batchMaxSize int) (int, error) {
		if deliver == nil {
			return 0, errors.New("logger batch delivery function is nil")
		}
		return deliver(entries, batchMaxSize)
	})
}

func NewWithContext(config Config, deliver ContextDeliveryFunc) (*Processor, error) {
	config.applyDefaults()
	if config.Tasks == nil {
		return nil, runtime.ErrTaskOwnerRequired
	}
	p := &Processor{
		config:  config,
		deliver: deliver,
		observer: metrics.AcquireLoggerBatchObserver(
			config.PluginID,
			config.Name,
			config.RouteID,
			config.ServerAddr,
		),
		workerCount:    config.MaxConcurrentDeliveries,
		active:         make(map[*workBatch]struct{}),
		buffer:         make([]map[string]any, 0, minInt(config.BatchMaxSize, DefaultBatchMaxSize)),
		wakeScheduler:  make(chan struct{}, 1),
		admissionDone:  make(chan struct{}),
		schedulerReady: make(chan struct{}),
		schedulerDone:  make(chan struct{}),
		shutdownDone:   make(chan struct{}),
		workersDone:    make(chan struct{}),
	}
	p.cond = sync.NewCond(&p.mu)

	if err := config.Tasks.Go("batch-scheduler", p.schedulerTask); err != nil {
		p.rollbackUnadmitted()
		return nil, err
	}
	<-p.schedulerReady
	for i := 0; i < p.workerCount; i++ {
		if err := config.Tasks.Go("batch-worker", p.workerTask); err != nil {
			p.rollbackAdmission()
			return nil, err
		}
		p.mu.Lock()
		p.workersLeft++
		p.mu.Unlock()
	}
	if err := config.Tasks.Go("batch-shutdown", p.shutdownTask); err != nil {
		p.rollbackAdmission()
		return nil, err
	}
	p.admissionOnce.Do(func() { close(p.admissionDone) })
	return p, nil
}

func (p *Processor) Push(entry map[string]any) bool {
	p.mu.Lock()

	if p.stopped {
		p.dropped++
		p.observer.AddEvent(metrics.LoggerBatchOutcomeStoppedDropped)
		p.mu.Unlock()
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
		p.mu.Unlock()
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
		p.flushRequested = true
	}
	p.scheduleTimerLocked()
	p.mu.Unlock()
	return true
}

func (p *Processor) Flush() {
	p.mu.Lock()
	if !p.stopped {
		p.flushRequested = true
	}
	p.mu.Unlock()
	p.wakeSchedulerTask()
}

func (p *Processor) Stop() {
	p.StopWithCleanup(nil)
}

// StopWithCleanup seals scheduler admission before returning. Delivery-owned
// resources remain registered with the generation task owner until every
// already-admitted callback has exited.
func (p *Processor) StopWithCleanup(cleanup func()) {
	p.registerCleanup(cleanup)
	p.initiateShutdown()
	<-p.schedulerDone
}

func (p *Processor) registerCleanup(cleanup func()) {
	if cleanup == nil {
		return
	}
	p.mu.Lock()
	if p.cleanup == nil {
		p.cleanup = cleanup
	}
	registered := p.cleanup
	terminal := p.terminal
	p.mu.Unlock()
	if terminal {
		p.runCleanup(registered)
	}
}

func (p *Processor) runCleanup(cleanup func()) {
	if cleanup != nil {
		p.cleanupOnce.Do(cleanup)
	}
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
		return ctx.Err()
	}
}

func (p *Processor) shutdownResult() (error, bool) {
	p.mu.Lock()
	storedErr := p.shutdownErr
	p.mu.Unlock()
	if storedErr != nil {
		return storedErr, true
	}
	select {
	case <-p.shutdownDone:
		return nil, true
	default:
		return nil, false
	}
}

func (p *Processor) startShutdown() {
	p.initiateShutdown()
}

func (p *Processor) abort(reason error) {
	p.mu.Lock()
	select {
	case <-p.shutdownDone:
		p.mu.Unlock()
		return
	default:
	}
	if p.shutdownErr == nil {
		p.shutdownErr = reason
		if p.cancel != nil {
			p.cancel()
		}
		dropped := p.dropUndispatchedLocked()
		p.cond.Broadcast()
		if dropped > 0 {
			p.observer.AddEvent(metrics.LoggerBatchOutcomeStoppedDropped)
		}
		p.observer.AddEvent(metrics.LoggerBatchOutcomeShutdownTimeout)
	}
	p.mu.Unlock()
}

func (p *Processor) initiateShutdown() {
	p.mu.Lock()
	p.stopRequested = true
	p.mu.Unlock()
	p.wakeSchedulerTask()
}

func (p *Processor) workerTask(_ context.Context) error {
	<-p.admissionDone
	defer p.workerExited()
	p.mu.Lock()
	failed := p.admissionFailed
	p.mu.Unlock()
	if failed {
		return nil
	}
	for {
		batch := p.nextBatch()
		if batch == nil {
			return nil
		}
		p.process(batch)
	}
}

func (p *Processor) workerExited() {
	p.mu.Lock()
	p.workersLeft--
	last := p.workersLeft == 0
	p.mu.Unlock()
	if last {
		p.workersOnce.Do(func() { close(p.workersDone) })
	}
}

func (p *Processor) schedulerTask(ctx context.Context) error {
	p.mu.Lock()
	p.deliveryCtx, p.cancel = context.WithCancel(context.WithoutCancel(ctx))
	p.mu.Unlock()
	close(p.schedulerReady)
	<-p.admissionDone
	defer p.schedulerOnce.Do(func() { close(p.schedulerDone) })

	p.mu.Lock()
	failed := p.admissionFailed
	p.mu.Unlock()
	if failed {
		return nil
	}

	var timer *time.Timer
	for {
		p.mu.Lock()
		if p.stopRequested || ctx.Err() != nil {
			p.stopped = true
			p.flushRequested = false
			p.flushLocked()
			p.cond.Broadcast()
			p.mu.Unlock()
			stopSchedulerTimer(timer)
			return nil
		}
		if p.flushRequested || len(p.buffer) >= p.config.BatchMaxSize {
			p.flushRequested = false
			p.flushLocked()
			p.mu.Unlock()
			stopSchedulerTimer(timer)
			timer = nil
			continue
		}
		deadline, scheduled := p.nextDeadlineLocked()
		generation := p.timerGen
		p.mu.Unlock()

		if !scheduled {
			stopSchedulerTimer(timer)
			timer = nil
			select {
			case <-p.wakeScheduler:
			case <-ctx.Done():
			}
			continue
		}

		delay := max(time.Until(deadline), 0)
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			stopSchedulerTimer(timer)
			timer.Reset(delay)
		}
		select {
		case <-p.wakeScheduler:
		case <-timer.C:
			p.onTimer(generation)
		case <-ctx.Done():
		}
	}
}

func (p *Processor) shutdownTask(_ context.Context) error {
	<-p.admissionDone
	<-p.schedulerDone
	timer := time.NewTimer(p.config.ShutdownTimeout)
	select {
	case <-p.workersDone:
		if !timer.Stop() {
			<-timer.C
		}
	case <-timer.C:
		p.abort(context.DeadlineExceeded)
		<-p.workersDone
	}
	p.finishTerminal()
	return nil
}

func (p *Processor) finishTerminal() {
	p.shutdownOnce.Do(func() {
		p.mu.Lock()
		p.terminal = true
		cleanup := p.cleanup
		cancel := p.cancel
		p.mu.Unlock()
		p.runCleanup(cleanup)
		if cancel != nil {
			cancel()
		}
		p.observer.Close()
		close(p.shutdownDone)
	})
}

func (p *Processor) rollbackUnadmitted() {
	p.admissionFailed = true
	p.stopped = true
	p.admissionOnce.Do(func() { close(p.admissionDone) })
	p.schedulerOnce.Do(func() { close(p.schedulerDone) })
	p.workersOnce.Do(func() { close(p.workersDone) })
	p.finishTerminal()
}

func (p *Processor) rollbackAdmission() {
	p.mu.Lock()
	p.admissionFailed = true
	p.stopped = true
	p.stopRequested = true
	workers := p.workersLeft
	p.cond.Broadcast()
	p.mu.Unlock()
	p.admissionOnce.Do(func() { close(p.admissionDone) })
	p.wakeSchedulerTask()
	if workers == 0 {
		p.workersOnce.Do(func() { close(p.workersDone) })
	}
	<-p.schedulerDone
	<-p.workersDone
	p.finishTerminal()
}

func (p *Processor) wakeSchedulerTask() {
	select {
	case p.wakeScheduler <- struct{}{}:
	default:
	}
}

func (p *Processor) nextDeadlineLocked() (time.Time, bool) {
	if len(p.buffer) == 0 {
		return time.Time{}, false
	}
	deadline := p.firstEntry.Add(p.config.BufferDuration)
	inactiveDeadline := p.lastEntry.Add(p.config.InactiveTimeout)
	if inactiveDeadline.Before(deadline) {
		deadline = inactiveDeadline
	}
	return deadline, true
}

func stopSchedulerTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
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
	p.timerGen++
	p.wakeSchedulerTask()
}

func (p *Processor) onTimer(generation uint64) {
	p.mu.Lock()
	if generation != p.timerGen {
		p.mu.Unlock()
		return
	}
	if p.stopped || len(p.buffer) == 0 {
		p.mu.Unlock()
		return
	}
	deadline, _ := p.nextDeadlineLocked()
	if time.Now().Before(deadline) {
		p.mu.Unlock()
		p.wakeSchedulerTask()
		return
	}
	p.flushLocked()
	p.mu.Unlock()
}

func (p *Processor) flushLocked() {
	if len(p.buffer) == 0 {
		return
	}
	for len(p.buffer) > 0 {
		count := minInt(len(p.buffer), p.config.BatchMaxSize)
		batch := &workBatch{entries: append([]map[string]any(nil), p.buffer[:count]...)}
		p.buffer = p.buffer[count:]
		p.processing += len(batch.entries)
		p.ready = append(p.ready, batch)
	}
	p.buffer = p.buffer[:0]
	p.firstEntry = time.Time{}
	p.lastEntry = time.Time{}
	p.setBufferedMetricLocked()
	p.stopTimerLocked()
	p.cond.Broadcast()
}

func (p *Processor) setBufferedMetricLocked() {
	if p.config.Name != "" && p.config.RouteID != "" && p.config.ServerAddr != "" {
		metrics.SetBatchProcessEntries(p.config.Name, p.config.RouteID, p.config.ServerAddr, len(p.buffer))
	}
	p.observer.SetBuffered(len(p.buffer))
}

func (p *Processor) stopTimerLocked() {
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
