package otel

import (
	"context"
	"errors"
	"sync"
	"time"

	otelapi "go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/wklken/apisix-go/pkg/runtime"
)

const (
	defaultOTelMaxQueueSize       = 2048
	defaultOTelBatchTimeout       = 5 * time.Second
	defaultOTelInactiveTimeout    = 2 * time.Second
	defaultOTelMaxExportBatchSize = 256
)

type apisixBatchConfig struct {
	dropOnQueueFull    bool
	maxQueueSize       int
	batchTimeout       time.Duration
	inactiveTimeout    time.Duration
	maxExportBatchSize int
}

type batchProcessorRequest struct {
	ctx      context.Context
	shutdown bool
	done     chan error
}

type apisixBatchSpanProcessor struct {
	exporter sdktrace.SpanExporter
	config   apisixBatchConfig

	mu           sync.Mutex
	queue        []sdktrace.ReadOnlySpan
	ready        [][]sdktrace.ReadOnlySpan
	urgent       [][]sdktrace.ReadOnlySpan
	firstQueueAt time.Time
	timerActive  bool
	forceReady   bool
	closed       bool

	wake          chan struct{}
	quiesce       chan struct{}
	requests      chan batchProcessorRequest
	schedulerDone chan struct{}
	shutdownDone  chan struct{}
	shutdownOnce  sync.Once
	shutdownErr   error
	exportMu      sync.Mutex
}

var _ sdktrace.SpanProcessor = (*apisixBatchSpanProcessor)(nil)

func newAPISIXBatchSpanProcessor(
	exporter sdktrace.SpanExporter,
	configured BatchSpanProcessorConfig,
	tasks *runtime.TaskOwner,
) (*apisixBatchSpanProcessor, error) {
	if exporter == nil {
		return nil, errors.New("opentelemetry span exporter is required")
	}
	config, err := normalizeAPISIXBatchConfig(configured)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		return nil, runtime.ErrTaskOwnerRequired
	}
	processor := &apisixBatchSpanProcessor{
		exporter:      exporter,
		config:        config,
		queue:         make([]sdktrace.ReadOnlySpan, 0, config.maxExportBatchSize),
		wake:          make(chan struct{}, 1),
		quiesce:       make(chan struct{}, 1),
		requests:      make(chan batchProcessorRequest),
		schedulerDone: make(chan struct{}),
		shutdownDone:  make(chan struct{}),
	}
	if err := tasks.Go("batch-span-processor", processor.run); err != nil {
		return nil, err
	}
	return processor, nil
}

func (p *apisixBatchSpanProcessor) Quiesce() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	select {
	case p.quiesce <- struct{}{}:
	default:
	}
}

func normalizeAPISIXBatchConfig(config BatchSpanProcessorConfig) (apisixBatchConfig, error) {
	result := apisixBatchConfig{
		dropOnQueueFull:    true,
		maxQueueSize:       defaultOTelMaxQueueSize,
		batchTimeout:       defaultOTelBatchTimeout,
		inactiveTimeout:    defaultOTelInactiveTimeout,
		maxExportBatchSize: defaultOTelMaxExportBatchSize,
	}
	if config.DropOnQueueFull != nil {
		result.dropOnQueueFull = *config.DropOnQueueFull
	}
	if config.MaxQueueSize != nil {
		result.maxQueueSize = *config.MaxQueueSize
	}
	if config.BatchTimeout != nil {
		result.batchTimeout = time.Duration(*config.BatchTimeout * float64(time.Second))
	}
	if config.InactiveTimeout != nil {
		result.inactiveTimeout = time.Duration(*config.InactiveTimeout * float64(time.Second))
	}
	if config.MaxExportBatchSize != nil {
		result.maxExportBatchSize = *config.MaxExportBatchSize
	}
	if result.batchTimeout <= 0 {
		return apisixBatchConfig{}, errors.New("opentelemetry batch_timeout must be greater than 0")
	}
	if result.inactiveTimeout <= 0 {
		return apisixBatchConfig{}, errors.New("opentelemetry inactive_timeout must be greater than 0")
	}
	if result.maxExportBatchSize <= 0 {
		return apisixBatchConfig{}, errors.New("opentelemetry max_export_batch_size must be greater than 0")
	}
	if result.maxQueueSize <= result.maxExportBatchSize {
		return apisixBatchConfig{}, errors.New(
			"opentelemetry max_queue_size must be greater than max_export_batch_size",
		)
	}
	return result, nil
}

func (*apisixBatchSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *apisixBatchSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if p.pendingLocked() >= p.config.maxQueueSize {
		if p.config.dropOnQueueFull {
			p.mu.Unlock()
			return
		}
		p.urgent = append(p.urgent, p.ready...)
		p.ready = nil
		p.forceReady = len(p.urgent) > 0
	}
	if len(p.queue) == 0 {
		p.firstQueueAt = time.Now()
	}
	p.queue = append(p.queue, span)
	if len(p.queue) >= p.config.maxExportBatchSize {
		p.ready = append(p.ready, p.queue)
		p.queue = make([]sdktrace.ReadOnlySpan, 0, p.config.maxExportBatchSize)
		p.firstQueueAt = time.Time{}
	}
	startTimer := !p.timerActive
	if startTimer {
		p.timerActive = true
	}
	forceReady := p.forceReady
	p.mu.Unlock()
	if startTimer || forceReady {
		p.wakeScheduler()
	}
}

func (p *apisixBatchSpanProcessor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p.shutdownOnce.Do(func() {
		p.shutdownErr = p.request(ctx, true)
		close(p.shutdownDone)
	})
	select {
	case <-p.shutdownDone:
		return p.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *apisixBatchSpanProcessor) ForceFlush(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return p.request(ctx, false)
}

func (p *apisixBatchSpanProcessor) request(ctx context.Context, shutdown bool) error {
	request := batchProcessorRequest{ctx: ctx, shutdown: shutdown, done: make(chan error, 1)}
	select {
	case p.requests <- request:
	case <-p.schedulerDone:
		return p.finishWithoutScheduler(ctx, shutdown)
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *apisixBatchSpanProcessor) run(ctx context.Context) error {
	defer close(p.schedulerDone)
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			batches := p.takeAll(true)
			if err := p.exportBatches(context.WithoutCancel(ctx), batches); err != nil {
				otelapi.Handle(err)
			}
			return nil
		case <-p.quiesce:
			if timerC != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			batches := p.takeAll(true)
			if err := p.exportBatches(context.Background(), batches); err != nil {
				otelapi.Handle(err)
			}
			return nil
		case <-p.wake:
			batches, startTimer := p.takeForcedBatches()
			var err error
			startTimer, err = p.exportScheduledBatches(ctx, batches, startTimer)
			if err != nil {
				otelapi.Handle(err)
			}
			if timerC == nil && startTimer {
				resetBatchTimer(timer, p.config.inactiveTimeout)
				timerC = timer.C
			}
		case <-timerC:
			batches, hasWork := p.poll(time.Now())
			var err error
			hasWork, err = p.exportScheduledBatches(ctx, batches, hasWork)
			if err != nil {
				otelapi.Handle(err)
			}
			if hasWork {
				resetBatchTimer(timer, p.config.inactiveTimeout)
				timerC = timer.C
			} else {
				timerC = nil
			}
		case request := <-p.requests:
			if timerC != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerC = nil
			batches := p.takeAll(request.shutdown)
			err := p.exportBatches(request.ctx, batches)
			if request.shutdown {
				err = errors.Join(err, p.shutdownExporter(request.ctx))
			}
			request.done <- err
			if request.shutdown {
				return nil
			}
		}
	}
}

func (p *apisixBatchSpanProcessor) takeForcedBatches() ([][]sdktrace.ReadOnlySpan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var batches [][]sdktrace.ReadOnlySpan
	if p.forceReady {
		batches = p.urgent
		p.urgent = nil
		p.forceReady = false
	}
	return batches, p.timerActive
}

func (p *apisixBatchSpanProcessor) poll(now time.Time) ([][]sdktrace.ReadOnlySpan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) > 0 && now.Sub(p.firstQueueAt) >= p.config.batchTimeout {
		p.ready = append(p.ready, p.queue)
		p.queue = make([]sdktrace.ReadOnlySpan, 0, p.config.maxExportBatchSize)
		p.firstQueueAt = time.Time{}
	}
	batches := p.ready
	p.ready = nil
	hasWork := len(p.queue) > 0
	p.timerActive = hasWork
	return batches, hasWork
}

func (p *apisixBatchSpanProcessor) exportScheduledBatches(
	ctx context.Context,
	batches [][]sdktrace.ReadOnlySpan,
	hasWork bool,
) (bool, error) {
	if len(batches) == 0 {
		return hasWork, nil
	}
	var result error
	for len(batches) > 0 {
		result = errors.Join(result, p.exportBatches(ctx, batches))
		batches, hasWork = p.takeReadyAfterExport()
	}
	return hasWork, result
}

func (p *apisixBatchSpanProcessor) takeReadyAfterExport() ([][]sdktrace.ReadOnlySpan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	batches := append(p.urgent, p.ready...)
	p.urgent = nil
	p.ready = nil
	p.forceReady = false
	p.timerActive = len(p.queue) > 0
	return batches, p.timerActive
}

func (p *apisixBatchSpanProcessor) takeAll(shutdown bool) [][]sdktrace.ReadOnlySpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	if shutdown {
		p.closed = true
	}
	if len(p.queue) > 0 {
		p.ready = append(p.ready, p.queue)
		p.queue = nil
	}
	batches := append(p.urgent, p.ready...)
	p.urgent = nil
	p.ready = nil
	p.firstQueueAt = time.Time{}
	p.timerActive = false
	p.forceReady = false
	return batches
}

func (p *apisixBatchSpanProcessor) finishWithoutScheduler(ctx context.Context, shutdown bool) error {
	batches := p.takeAll(shutdown)
	err := p.exportBatches(ctx, batches)
	if shutdown {
		err = errors.Join(err, p.shutdownExporter(ctx))
	}
	return err
}

func (p *apisixBatchSpanProcessor) exportBatches(
	ctx context.Context,
	batches [][]sdktrace.ReadOnlySpan,
) error {
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	var result error
	for _, batch := range batches {
		if len(batch) == 0 {
			continue
		}
		result = errors.Join(result, p.exporter.ExportSpans(ctx, batch))
	}
	return result
}

func (p *apisixBatchSpanProcessor) shutdownExporter(ctx context.Context) error {
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	return p.exporter.Shutdown(ctx)
}

func (p *apisixBatchSpanProcessor) pendingLocked() int {
	pending := len(p.queue)
	for _, batch := range p.ready {
		pending += len(batch)
	}
	return pending
}

func (p *apisixBatchSpanProcessor) wakeScheduler() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func resetBatchTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}
