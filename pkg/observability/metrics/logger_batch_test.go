package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestLoggerBatchObserverIsNilSafeBeforeInit(t *testing.T) {
	oldPending, oldEvents, oldBuffered := LoggerBatchPendingEntries, LoggerBatchEvents, BatchProcessEntries
	LoggerBatchPendingEntries, LoggerBatchEvents, BatchProcessEntries = nil, nil, nil
	t.Cleanup(func() {
		LoggerBatchPendingEntries, LoggerBatchEvents, BatchProcessEntries = oldPending, oldEvents, oldBuffered
	})

	observer := AcquireLoggerBatchObserver("http-logger", "batch", "route", "server")
	observer.SetBuffered(2)
	observer.AddPending(2)
	if !observer.AddEvent(LoggerBatchOutcomeCapacityDropped) {
		t.Fatal("valid event was rejected before metrics initialization")
	}
	if observer.AddEvent("unknown") {
		t.Fatal("unknown event was accepted")
	}
	observer.Close()
}

func TestLoggerBatchObserverAggregatesOverlappingGenerations(t *testing.T) {
	oldPending, oldEvents, oldBuffered := LoggerBatchPendingEntries, LoggerBatchEvents, BatchProcessEntries
	LoggerBatchPendingEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_logger_batch_pending"},
		[]string{"plugin", "route_id", "server_addr"},
	)
	LoggerBatchEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_logger_batch_events"},
		[]string{"plugin", "outcome"},
	)
	BatchProcessEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_logger_batch_buffered"},
		[]string{"name", "route_id", "server_addr"},
	)
	t.Cleanup(func() {
		LoggerBatchPendingEntries, LoggerBatchEvents, BatchProcessEntries = oldPending, oldEvents, oldBuffered
	})

	first := AcquireLoggerBatchObserver("http-logger", "batch", "route", "server")
	second := AcquireLoggerBatchObserver("http-logger", "batch", "route", "server")
	first.SetBuffered(2)
	second.SetBuffered(3)
	first.AddPending(2)
	second.AddPending(3)
	if got := metricGaugeValue(
		t,
		LoggerBatchPendingEntries.WithLabelValues("http-logger", "route", "server"),
	); got != 5 {
		t.Fatalf("pending gauge = %v, want 5", got)
	}
	if got := metricGaugeValue(t, BatchProcessEntries.WithLabelValues("batch", "route", "server")); got != 5 {
		t.Fatalf("buffered gauge = %v, want 5", got)
	}

	first.Close()
	if got := metricGaugeValue(
		t,
		LoggerBatchPendingEntries.WithLabelValues("http-logger", "route", "server"),
	); got != 3 {
		t.Fatalf("pending after old-generation close = %v, want 3", got)
	}
	if got := metricGaugeValue(t, BatchProcessEntries.WithLabelValues("batch", "route", "server")); got != 3 {
		t.Fatalf("buffered after old-generation close = %v, want 3", got)
	}
	second.Close()
	if got := gatheredMetricCount(t, LoggerBatchPendingEntries); got != 0 {
		t.Fatalf("pending series after last close = %d, want 0", got)
	}
	if got := gatheredMetricCount(t, BatchProcessEntries); got != 0 {
		t.Fatalf("buffered series after last close = %d, want 0", got)
	}
}

func TestLoggerBatchObserverCountersPersistAndValidateLabels(t *testing.T) {
	oldEvents := LoggerBatchEvents
	LoggerBatchEvents = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_logger_batch_events_persist"},
		[]string{"plugin", "outcome"},
	)
	t.Cleanup(func() { LoggerBatchEvents = oldEvents })

	first := AcquireLoggerBatchObserver("zipkin", "batch", "route", "server")
	if !first.AddEvent(LoggerBatchOutcomeDeliveryFailed) {
		t.Fatal("delivery failure event was rejected")
	}
	first.Close()
	second := AcquireLoggerBatchObserver("zipkin", "batch", "route", "server")
	if !second.AddEvent(LoggerBatchOutcomeDeliveryFailed) {
		t.Fatal("second delivery failure event was rejected")
	}
	if got := metricCounterValue(
		t,
		LoggerBatchEvents.WithLabelValues("zipkin", LoggerBatchOutcomeDeliveryFailed),
	); got != 2 {
		t.Fatalf("delivery failure counter = %v, want 2", got)
	}
	second.Close()

	invalid := AcquireLoggerBatchObserver("not-a-production-plugin", "batch", "route", "server")
	if invalid.AddEvent(LoggerBatchOutcomeDeliveryFailed) {
		t.Fatal("invalid plugin label was accepted")
	}
}

func metricGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func metricCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func gatheredMetricCount(t *testing.T, collector prometheus.Collector) int {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	if len(families) == 0 {
		return 0
	}
	return len(families[0].Metric)
}
