package logger_batch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

func TestProcessorFlushesWhenBatchMaxSizeIsReached(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    2,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 2 {
		t.Fatalf("batch length = %d, want 2", len(batch))
	}
	if batch[0]["id"] != 1 || batch[1]["id"] != 2 {
		t.Fatalf("batch = %#v, want ids 1 and 2", batch)
	}
}

func TestProcessorFlushesAfterInactiveTimeout(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: 20 * time.Millisecond,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": "timeout"}) {
		t.Fatal("push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 1 || batch[0]["id"] != "timeout" {
		t.Fatalf("batch = %#v, want timeout entry", batch)
	}
}

func TestProcessorStopFlushesBufferedEntries(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})

	if !p.Push(map[string]any{"id": "stop"}) {
		t.Fatal("push was rejected")
	}

	p.Stop()
	batch := waitBatch(t, delivered)
	if len(batch) != 1 || batch[0]["id"] != "stop" {
		t.Fatalf("batch = %#v, want stop entry", batch)
	}
}

func TestProcessorDropsEntriesPastMaxPendingEntries(t *testing.T) {
	block := make(chan struct{})
	p := New(Config{
		Name:              "test logger",
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		<-block
		return 0, nil
	})
	t.Cleanup(func() {
		close(block)
		p.Stop()
	})

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	if p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was accepted at max_pending_entries")
	}
	if p.Push(map[string]any{"id": 3}) {
		t.Fatal("third push was accepted after max_pending_entries was reached")
	}
}

func TestProcessorUpdatesBatchProcessEntriesMetric(t *testing.T) {
	oldBatchProcessEntries := metrics.BatchProcessEntries
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_batch_process_entries"},
		[]string{"name", "route_id", "server_addr"},
	)
	metrics.BatchProcessEntries = gauge
	t.Cleanup(func() {
		metrics.BatchProcessEntries = oldBatchProcessEntries
	})

	delivered := make(chan []map[string]any, 1)
	p := New(Config{
		Name:            "http logger",
		RouteID:         "route-a",
		ServerAddr:      "127.0.0.1:9080",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	if got := gaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 1 {
		t.Fatalf("batch_process_entries = %v, want 1 after first push", got)
	}

	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was rejected")
	}
	if got := gaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 2 {
		t.Fatalf("batch_process_entries = %v, want 2 after second push", got)
	}

	p.Flush()
	_ = waitBatch(t, delivered)
	if got := gaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 0 {
		t.Fatalf("batch_process_entries = %v, want 0 after flush", got)
	}
}

func TestProcessorSkipsBatchProcessEntriesMetricWithoutRouteContext(t *testing.T) {
	oldBatchProcessEntries := metrics.BatchProcessEntries
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_batch_process_entries_without_route"},
		[]string{"name", "route_id", "server_addr"},
	)
	metrics.BatchProcessEntries = gauge
	t.Cleanup(func() {
		metrics.BatchProcessEntries = oldBatchProcessEntries
	})

	p := New(Config{
		Name:            "error log logger",
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("push was rejected")
	}
	if got := gaugeValue(t, gauge, "error log logger", "", ""); got != 0 {
		t.Fatalf("batch_process_entries with empty route context = %v, want 0", got)
	}
}

func gaugeValue(t *testing.T, gauge *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := gauge.WithLabelValues(labels...).Write(metric); err != nil {
		t.Fatalf("read gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func TestProcessorRetriesFailedBatches(t *testing.T) {
	delivered := make(chan []map[string]any, 1)
	attempts := 0
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    1,
		MaxRetryCount:   1,
		RetryDelay:      10 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(entries []map[string]any, _ int) (int, error) {
		attempts++
		if attempts == 1 {
			return 0, fmt.Errorf("temporary failure")
		}
		delivered <- entries
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": "retry"}) {
		t.Fatal("push was rejected")
	}

	batch := waitBatch(t, delivered)
	if len(batch) != 1 || batch[0]["id"] != "retry" {
		t.Fatalf("batch = %#v, want retry entry", batch)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestProcessorPreservesExplicitZeroRetryDelay(t *testing.T) {
	attempts := make(chan time.Time, 3)
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    1,
		MaxRetryCount:   2,
		RetryDelay:      0,
		RetryDelaySet:   true,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		attempts <- time.Now()
		return 0, fmt.Errorf("deterministic failure")
	})
	t.Cleanup(p.Stop)

	if !p.Push(map[string]any{"id": "zero-delay"}) {
		t.Fatal("push was rejected")
	}

	var first time.Time
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case at := <-attempts:
			if attempt == 1 {
				first = at
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("attempts = %d, want 3 without the default one-second retry delay", attempt-1)
		}
	}
	if elapsed := time.Since(first); elapsed >= 250*time.Millisecond {
		t.Fatalf("three attempts took %s, want explicit zero retry delay", elapsed)
	}
}

func TestProcessorPushDoesNotWaitForDelivery(t *testing.T) {
	block := make(chan struct{})
	p := New(Config{
		Name:            "test logger",
		BatchMaxSize:    1,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ []map[string]any, _ int) (int, error) {
		<-block
		return 0, nil
	})
	t.Cleanup(func() {
		close(block)
		p.Stop()
	})

	done := make(chan bool, 1)
	go func() {
		done <- p.Push(map[string]any{"id": "non-blocking"})
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("push was rejected")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("push blocked on delivery")
	}
}

func TestProcessorPushDoesNotWaitBehindActiveAndQueuedDeliveries(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p := NewWithContext(Config{
		BatchMaxSize:      1,
		MaxPendingEntries: 4,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return 0, nil
	})
	t.Cleanup(func() {
		close(release)
		p.Stop()
	})

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}
	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("queued push was rejected")
	}

	done := make(chan bool, 1)
	go func() {
		done <- p.Push(map[string]any{"id": 3})
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("push behind queued delivery was rejected")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("push blocked behind active and queued deliveries")
	}
}

func TestProcessorDefaultsResourceBounds(t *testing.T) {
	p := NewWithContext(Config{}, func(context.Context, []map[string]any, int) (int, error) {
		return 0, nil
	})
	t.Cleanup(p.Stop)

	if p.config.MaxPendingEntries != DefaultMaxPendingEntries {
		t.Fatalf("MaxPendingEntries = %d, want %d", p.config.MaxPendingEntries, DefaultMaxPendingEntries)
	}
	if p.config.MaxConcurrentDeliveries != DefaultMaxConcurrentDeliveries {
		t.Fatalf(
			"MaxConcurrentDeliveries = %d, want %d",
			p.config.MaxConcurrentDeliveries,
			DefaultMaxConcurrentDeliveries,
		)
	}
	if p.config.DeliveryTimeout != DefaultDeliveryTimeout {
		t.Fatalf("DeliveryTimeout = %s, want %s", p.config.DeliveryTimeout, DefaultDeliveryTimeout)
	}
	if p.config.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %s, want %s", p.config.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if p.workerCount != DefaultMaxConcurrentDeliveries {
		t.Fatalf("workerCount = %d, want %d", p.workerCount, DefaultMaxConcurrentDeliveries)
	}
}

func TestProcessorBoundsInitialBufferCapacityWithoutChangingBatchSize(t *testing.T) {
	const configuredBatchMaxSize = 1 << 60
	deliveredBatchSize := make(chan int, 1)
	var p *Processor

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("NewWithContext panicked for a large configured batch size: %v", recovered)
			}
		}()
		p = NewWithContext(Config{
			BatchMaxSize:    configuredBatchMaxSize,
			InactiveTimeout: time.Hour,
			BufferDuration:  time.Hour,
		}, func(_ context.Context, _ []map[string]any, batchMaxSize int) (int, error) {
			deliveredBatchSize <- batchMaxSize
			return 0, nil
		})
	}()
	if p == nil {
		t.Fatal("NewWithContext returned a nil processor")
	}
	t.Cleanup(p.Stop)

	if p.config.BatchMaxSize != configuredBatchMaxSize {
		t.Fatalf("configured BatchMaxSize = %d, want %d", p.config.BatchMaxSize, configuredBatchMaxSize)
	}
	if got := cap(p.buffer); got > DefaultBatchMaxSize {
		t.Fatalf("initial buffer capacity = %d, want at most %d", got, DefaultBatchMaxSize)
	}
	if !p.Push(map[string]any{"id": "bounded-capacity"}) {
		t.Fatal("push was rejected")
	}

	p.Stop()
	select {
	case got := <-deliveredBatchSize:
		if got != configuredBatchMaxSize {
			t.Fatalf("delivery batch size = %d, want configured value %d", got, configuredBatchMaxSize)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

func TestProcessorRejectsAtExactPendingBoundary(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	p := NewWithContext(Config{
		BatchMaxSize:      1,
		MaxPendingEntries: 1,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		close(started)
		<-release
		return 0, nil
	})
	t.Cleanup(func() {
		close(release)
		p.Stop()
	})

	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}
	if p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was accepted at exact pending boundary")
	}
	if got := p.Stats().Pending; got != 1 {
		t.Fatalf("pending = %d, want 1 after boundary rejection", got)
	}
}

func TestProcessorBoundsConcurrentDeliveries(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	p := NewWithContext(Config{
		BatchMaxSize:            1,
		MaxPendingEntries:       3,
		MaxConcurrentDeliveries: 2,
		InactiveTimeout:         time.Hour,
		BufferDuration:          time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		current := active.Add(1)
		started <- struct{}{}
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		<-release
		active.Add(-1)
		return 0, nil
	})
	t.Cleanup(func() {
		close(release)
		p.Stop()
	})

	for i := range 3 {
		if !p.Push(map[string]any{"id": i}) {
			t.Fatalf("push %d was rejected", i)
		}
	}
	for i := range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("delivery %d did not start", i+1)
		}
	}
	if got := active.Load(); got != 2 {
		t.Fatalf("active deliveries = %d, want exactly 2 before release", got)
	}
	select {
	case <-started:
		t.Fatal("third delivery started before a worker was released")
	case <-time.After(50 * time.Millisecond):
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("max active deliveries = %d, want exactly 2", got)
	}
}

func TestProcessorDefaultConcurrencyIsOne(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	p := NewWithContext(Config{
		BatchMaxSize:      1,
		MaxPendingEntries: 2,
		InactiveTimeout:   time.Hour,
		BufferDuration:    time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		started <- struct{}{}
		<-release
		return 0, nil
	})
	t.Cleanup(func() {
		close(release)
		p.Stop()
	})

	if !p.Push(map[string]any{"id": 1}) || !p.Push(map[string]any{"id": 2}) {
		t.Fatal("push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first delivery did not start")
	}
	select {
	case <-started:
		t.Fatal("second delivery started before the first was released")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProcessorDeliveryTimeoutAndShutdownAccounting(t *testing.T) {
	t.Run("delivery timeout", func(t *testing.T) {
		p := NewWithContext(Config{
			BatchMaxSize:    1,
			DeliveryTimeout: 20 * time.Millisecond,
			InactiveTimeout: time.Hour,
			BufferDuration:  time.Hour,
		}, func(ctx context.Context, _ []map[string]any, _ int) (int, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		})
		t.Cleanup(p.Stop)
		if !p.Push(map[string]any{"id": "timeout"}) {
			t.Fatal("push was rejected")
		}
		deadline := time.Now().Add(time.Second)
		for p.Stats().FailedDrops == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		stats := p.Stats()
		if stats.Pending != 0 || stats.FailedDrops != 1 || stats.Delivered != 0 {
			t.Fatalf("timeout stats = %+v, want one terminal failed drop", stats)
		}
	})

	t.Run("shutdown deadline terminalizes active and queued entries once", func(t *testing.T) {
		started := make(chan struct{})
		p := NewWithContext(Config{
			BatchMaxSize:      1,
			MaxPendingEntries: 3,
			InactiveTimeout:   time.Hour,
			BufferDuration:    time.Hour,
		}, func(context.Context, []map[string]any, int) (int, error) {
			select {
			case <-started:
			default:
				close(started)
			}
			time.Sleep(100 * time.Millisecond)
			return 0, nil
		})
		for i := range 3 {
			if !p.Push(map[string]any{"id": i}) {
				t.Fatalf("push %d was rejected", i)
			}
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("active delivery did not start")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := p.Shutdown(ctx); err == nil {
			t.Fatal("Shutdown() error = nil, want deadline error")
		}
		if elapsed := time.Since(start); elapsed >= 90*time.Millisecond {
			t.Fatalf("Shutdown() took %s, want bounded return", elapsed)
		}
		stats := p.Stats()
		if stats.Pending != 0 || stats.Processing != 0 || stats.FailedDrops != 3 || stats.Delivered != 0 {
			t.Fatalf("shutdown stats = %+v, want exactly three terminal drops", stats)
		}
		if err := p.Shutdown(context.Background()); err == nil {
			t.Fatal("repeated Shutdown() error = nil, want original deadline error")
		}
		for range 2 {
			done := make(chan error, 1)
			go func() { done <- p.Shutdown(context.Background()) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("concurrent Shutdown() error = nil, want original deadline error")
				}
			case <-time.After(time.Second):
				t.Fatal("concurrent Shutdown() blocked")
			}
		}
		time.Sleep(150 * time.Millisecond)
		if got := p.Stats().FailedDrops; got != 3 {
			t.Fatalf("late callback changed failed drops to %d, want 3", got)
		}
	})
}

func TestProcessorDefersResourceCleanupUntilCallbackExits(t *testing.T) {
	started := make(chan struct{})
	releaseCallback := make(chan struct{})
	cleanupDone := make(chan struct{})
	p := NewWithContext(Config{
		BatchMaxSize:    1,
		ShutdownTimeout: 20 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(ctx context.Context, _ []map[string]any, _ int) (int, error) {
		close(started)
		<-ctx.Done()
		<-releaseCallback
		return 0, ctx.Err()
	})
	if !p.Push(map[string]any{"id": "cleanup"}) {
		t.Fatal("push was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		p.StopWithCleanup(func() { close(cleanupDone) })
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("StopWithCleanup did not return within the shutdown bound")
	}
	select {
	case <-cleanupDone:
		t.Fatal("resource cleanup ran before the active callback exited")
	default:
	}
	close(releaseCallback)
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("resource cleanup did not run after the callback exited")
	}
}

func TestProcessorRepeatedSuccessfulShutdownIgnoresNewCanceledContext(t *testing.T) {
	p := NewWithContext(Config{}, func(context.Context, []map[string]any, int) (int, error) {
		return 0, nil
	})
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := range 100 {
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("repeated Shutdown() %d error = %v, want stored nil result", i, err)
		}
	}
}

func TestProcessorCancelsRetryDelayDuringShutdown(t *testing.T) {
	attempted := make(chan struct{}, 1)
	p := NewWithContext(Config{
		BatchMaxSize:    1,
		MaxRetryCount:   2,
		RetryDelay:      time.Hour,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		attempted <- struct{}{}
		return 0, errors.New("retryable")
	})
	if !p.Push(map[string]any{"id": "retry"}) {
		t.Fatal("push was rejected")
	}
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("first delivery attempt did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v, want deadline exceeded", err)
	}
	stats := p.Stats()
	if stats.Pending != 0 || stats.FailedDrops != 1 {
		t.Fatalf("shutdown during retry stats = %+v, want one terminal failed drop", stats)
	}
	select {
	case <-attempted:
		t.Fatal("delivery retried after shutdown cancellation")
	default:
	}
}

func TestProcessorClassifiesOwnedDeadlineAsDeliveryTimeout(t *testing.T) {
	oldEvents := metrics.LoggerBatchEvents
	events := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_logger_batch_delivery_events_total"},
		[]string{"plugin", "outcome"},
	)
	metrics.LoggerBatchEvents = events
	t.Cleanup(func() { metrics.LoggerBatchEvents = oldEvents })

	p := NewWithContext(Config{
		Name:            "tcp logger",
		PluginID:        "tcp-logger",
		BatchMaxSize:    1,
		DeliveryTimeout: 20 * time.Millisecond,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(ctx context.Context, _ []map[string]any, _ int) (int, error) {
		<-ctx.Done()
		return 0, fmt.Errorf("socket write: %w", os.ErrDeadlineExceeded)
	})
	if !p.Push(map[string]any{"id": "timeout"}) {
		t.Fatal("push was rejected")
	}
	deadline := time.Now().Add(time.Second)
	for p.Stats().FailedDrops == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if got := counterValue(t, events, "tcp-logger", metrics.LoggerBatchOutcomeDeliveryTimeout); got != 1 {
		t.Fatalf("delivery_timeout events = %v, want 1", got)
	}
	if got := counterValue(t, events, "tcp-logger", metrics.LoggerBatchOutcomeDeliveryFailed); got != 0 {
		t.Fatalf("delivery_failed events = %v, want 0", got)
	}
}

func TestProcessorPreservesPartialSuccessSuffixAccounting(t *testing.T) {
	p := NewWithContext(Config{
		BatchMaxSize:    2,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		if len(entries) != 2 {
			t.Fatalf("retry entries = %d, want original batch of 2", len(entries))
		}
		return 2, fmt.Errorf("suffix failure")
	})
	t.Cleanup(p.Stop)
	if !p.Push(map[string]any{"id": 1}) || !p.Push(map[string]any{"id": 2}) {
		t.Fatal("push was rejected")
	}
	deadline := time.Now().Add(time.Second)
	for p.Stats().Pending != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stats := p.Stats()
	if stats.Delivered != 1 || stats.FailedDrops != 1 || stats.Pending != 0 {
		t.Fatalf("partial-success stats = %+v, want one delivered and one dropped", stats)
	}
}

func TestProcessorReschedulesInactivityAndHonorsBufferCeiling(t *testing.T) {
	t.Run("inactivity reschedules from latest push", func(t *testing.T) {
		delivered := make(chan struct{}, 1)
		p := NewWithContext(Config{
			BatchMaxSize:    10,
			InactiveTimeout: 60 * time.Millisecond,
			BufferDuration:  time.Second,
		}, func(context.Context, []map[string]any, int) (int, error) {
			delivered <- struct{}{}
			return 0, nil
		})
		t.Cleanup(p.Stop)
		if !p.Push(map[string]any{"id": 1}) {
			t.Fatal("first push was rejected")
		}
		time.Sleep(30 * time.Millisecond)
		if !p.Push(map[string]any{"id": 2}) {
			t.Fatal("second push was rejected")
		}
		select {
		case <-delivered:
			t.Fatal("batch flushed before inactivity deadline from latest push")
		case <-time.After(20 * time.Millisecond):
		}
		select {
		case <-delivered:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("batch did not flush after rescheduled inactivity deadline")
		}
	})

	t.Run("buffer duration is a hard ceiling", func(t *testing.T) {
		delivered := make(chan struct{}, 1)
		p := NewWithContext(Config{
			BatchMaxSize:    10,
			InactiveTimeout: 200 * time.Millisecond,
			BufferDuration:  50 * time.Millisecond,
		}, func(context.Context, []map[string]any, int) (int, error) {
			delivered <- struct{}{}
			return 0, nil
		})
		t.Cleanup(p.Stop)
		if !p.Push(map[string]any{"id": "ceiling"}) {
			t.Fatal("push was rejected")
		}
		select {
		case <-delivered:
		case <-time.After(150 * time.Millisecond):
			t.Fatal("batch did not flush at hard buffer-duration ceiling")
		}
	})
}

func TestProcessorIgnoresObsoleteTimerGenerationAfterReschedule(t *testing.T) {
	delivered := make(chan struct{}, 1)
	p := NewWithContext(Config{
		BatchMaxSize:    10,
		InactiveTimeout: time.Hour,
		BufferDuration:  time.Hour,
	}, func(context.Context, []map[string]any, int) (int, error) {
		delivered <- struct{}{}
		return 0, nil
	})
	t.Cleanup(p.Stop)
	if !p.Push(map[string]any{"id": 1}) {
		t.Fatal("first push was rejected")
	}
	p.mu.Lock()
	obsoleteGeneration := p.timerGen
	p.mu.Unlock()
	if !p.Push(map[string]any{"id": 2}) {
		t.Fatal("second push was rejected")
	}
	p.onTimer(obsoleteGeneration)
	stats := p.Stats()
	if stats.Buffered != 2 || stats.Pending != 2 {
		t.Fatalf("stats after obsolete timer = %+v, want two buffered pending entries", stats)
	}
	select {
	case <-delivered:
		t.Fatal("obsolete timer flushed the rescheduled batch")
	default:
	}
}

func counterValue(t *testing.T, counter *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.WithLabelValues(labels...).Write(metric); err != nil {
		t.Fatalf("read counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func waitBatch(t *testing.T, delivered <-chan []map[string]any) []map[string]any {
	t.Helper()

	select {
	case batch := <-delivered:
		return batch
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delivered batch")
	}
	return nil
}
