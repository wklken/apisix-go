package otel

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/wklken/apisix-go/pkg/runtime"
)

type signalingSpanExporter struct {
	exported chan int
	shutdown atomic.Int32
}

type blockingFirstSpanExporter struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	second  chan struct{}
}

func (e *blockingFirstSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	if e.calls.Add(1) == 1 {
		close(e.started)
		<-e.release
		return nil
	}
	close(e.second)
	return nil
}

func (*blockingFirstSpanExporter) Shutdown(context.Context) error { return nil }

func (e *signalingSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.exported <- len(spans)
	return nil
}

func (e *signalingSpanExporter) Shutdown(context.Context) error {
	e.shutdown.Add(1)
	return nil
}

func TestAPISIXBatchSpanProcessorPollsFromFirstQueuedSpan(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/opentelemetry/test", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	exporter := &signalingSpanExporter{exported: make(chan int, 1)}
	processor, err := newAPISIXBatchSpanProcessor(exporter, BatchSpanProcessorConfig{
		MaxQueueSize:       new(8),
		BatchTimeout:       new(0.01),
		InactiveTimeout:    new(0.08),
		MaxExportBatchSize: new(4),
	}, owner)
	if err != nil {
		t.Fatalf("newAPISIXBatchSpanProcessor() error = %v", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(processor),
	)

	// APISIX starts the inactive timer when the first span enters an empty
	// queue, not when the processor is created.
	time.Sleep(120 * time.Millisecond)
	_, span := provider.Tracer("test").Start(context.Background(), "request")
	span.End()

	select {
	case count := <-exporter.exported:
		t.Fatalf("exported %d span(s) before inactive_timeout", count)
	case <-time.After(40 * time.Millisecond):
	}

	select {
	case count := <-exporter.exported:
		if count != 1 {
			t.Fatalf("exported span count = %d, want 1", count)
		}
	case <-time.After(400 * time.Millisecond):
		t.Fatal("timed out waiting for inactive_timeout poll to export span")
	}

	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("TracerProvider.Shutdown() error = %v", err)
	}
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
}

func TestNormalizeAPISIXBatchConfigUsesOpenTelemetryLuaDefaults(t *testing.T) {
	config, err := normalizeAPISIXBatchConfig(BatchSpanProcessorConfig{})
	if err != nil {
		t.Fatalf("normalizeAPISIXBatchConfig() error = %v", err)
	}
	if !config.dropOnQueueFull || config.maxQueueSize != 2048 ||
		config.batchTimeout != 5*time.Second || config.inactiveTimeout != 2*time.Second ||
		config.maxExportBatchSize != 256 {
		t.Fatalf("normalized defaults = %#v", config)
	}
}

func TestNormalizeAPISIXBatchConfigPreservesExplicitFalseAndRejectsExplicitZero(t *testing.T) {
	config, err := normalizeAPISIXBatchConfig(BatchSpanProcessorConfig{
		DropOnQueueFull: new(false),
	})
	if err != nil {
		t.Fatalf("normalizeAPISIXBatchConfig(false) error = %v", err)
	}
	if config.dropOnQueueFull {
		t.Fatal("drop_on_queue_full = true, want explicit false")
	}

	for _, test := range []struct {
		name   string
		config BatchSpanProcessorConfig
	}{
		{name: "batch timeout", config: BatchSpanProcessorConfig{BatchTimeout: new(0.0)}},
		{name: "inactive timeout", config: BatchSpanProcessorConfig{InactiveTimeout: new(0.0)}},
		{name: "max export batch size", config: BatchSpanProcessorConfig{MaxExportBatchSize: new(0)}},
		{name: "queue not larger than batch", config: BatchSpanProcessorConfig{
			MaxQueueSize: new(16), MaxExportBatchSize: new(16),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeAPISIXBatchConfig(test.config); err == nil {
				t.Fatal("normalizeAPISIXBatchConfig() error = nil")
			}
		})
	}
}

func TestAPISIXBatchSpanProcessorDropFalseDetachesReadyBatchesBeforeAdmission(t *testing.T) {
	config, err := normalizeAPISIXBatchConfig(BatchSpanProcessorConfig{
		DropOnQueueFull:    new(false),
		MaxQueueSize:       new(3),
		MaxExportBatchSize: new(2),
	})
	if err != nil {
		t.Fatalf("normalizeAPISIXBatchConfig() error = %v", err)
	}
	processor := &apisixBatchSpanProcessor{
		config: config,
		queue:  make([]sdktrace.ReadOnlySpan, 0, config.maxExportBatchSize),
		wake:   make(chan struct{}, 1),
	}
	span := tracetest.SpanStub{SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{1},
		TraceFlags: trace.FlagsSampled,
	})}.Snapshot()
	for range 4 {
		processor.OnEnd(span)
	}

	processor.mu.Lock()
	pending := processor.pendingLocked()
	urgent := 0
	for _, batch := range processor.urgent {
		urgent += len(batch)
	}
	processor.mu.Unlock()
	if pending != 2 || urgent != 2 {
		t.Fatalf("queue ownership = pending:%d urgent:%d, want 2/2", pending, urgent)
	}
}

func TestAPISIXBatchSpanProcessorQuiesceDrainsBeforeExporterShutdown(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/opentelemetry/quiesce", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	exporter := &signalingSpanExporter{exported: make(chan int, 1)}
	processor, err := newAPISIXBatchSpanProcessor(exporter, BatchSpanProcessorConfig{}, owner)
	if err != nil {
		t.Fatalf("newAPISIXBatchSpanProcessor() error = %v", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(processor),
	)
	_, span := provider.Tracer("test").Start(context.Background(), "request")
	span.End()

	processor.Quiesce()
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
	select {
	case count := <-exporter.exported:
		if count != 1 {
			t.Fatalf("quiesce exported span count = %d, want 1", count)
		}
	case <-time.After(time.Second):
		t.Fatal("quiesce did not drain the queued span")
	}
	if got := exporter.shutdown.Load(); got != 0 {
		t.Fatalf("exporter shutdown count after quiesce = %d, want 0", got)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("TracerProvider.Shutdown() error = %v", err)
	}
	if got := exporter.shutdown.Load(); got != 1 {
		t.Fatalf("exporter shutdown count = %d, want 1", got)
	}
}

func TestAPISIXBatchSpanProcessorImmediatelyDrainsFullBatchCreatedDuringExport(t *testing.T) {
	tasks := runtime.NewTaskRegistry(context.Background(), nil)
	owner, err := runtime.NewTaskOwner(tasks, "plugin/opentelemetry/export-interleave", runtime.TaskPlugin)
	if err != nil {
		t.Fatalf("NewTaskOwner() error = %v", err)
	}
	exporter := &blockingFirstSpanExporter{
		started: make(chan struct{}),
		release: make(chan struct{}),
		second:  make(chan struct{}),
	}
	processor, err := newAPISIXBatchSpanProcessor(exporter, BatchSpanProcessorConfig{
		MaxQueueSize:       new(4),
		InactiveTimeout:    new(0.5),
		MaxExportBatchSize: new(1),
	}, owner)
	if err != nil {
		t.Fatalf("newAPISIXBatchSpanProcessor() error = %v", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(processor),
	)
	_, first := provider.Tracer("test").Start(context.Background(), "first")
	first.End()

	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("first full batch did not reach exporter")
	}
	_, second := provider.Tracer("test").Start(context.Background(), "second")
	second.End()
	close(exporter.release)

	select {
	case <-exporter.second:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("second full batch waited for another inactive_timeout")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("TracerProvider.Shutdown() error = %v", err)
	}
	if residuals, err := tasks.Stop(context.Background()); err != nil || len(residuals) != 0 {
		t.Fatalf("TaskRegistry.Stop() = (%v, %v)", residuals, err)
	}
}
