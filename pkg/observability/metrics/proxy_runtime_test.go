package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestProxyRuntimeObserverRecordsBoundedSignals(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer := newProxyRuntimeObserver(registry)

	observer.SetInFlight("orders", 1)
	observer.SetInFlight("orders", 1)
	observer.SetInFlight("orders", -1)
	observer.ObserveRejected("orders")
	observer.ObserveRetry("orders", "error")
	observer.SetHealth("orders", "127.0.0.1:8080", true)

	if got := gaugeVecValue(t, observer.inFlight.WithLabelValues("orders")); got != 1 {
		t.Fatalf("in-flight gauge = %v, want 1", got)
	}
	if got := counterVecValue(t, observer.rejected.WithLabelValues("orders")); got != 1 {
		t.Fatalf("rejected counter = %v, want 1", got)
	}
	if got := counterVecValue(t, observer.retry.WithLabelValues("orders", "error")); got != 1 {
		t.Fatalf("retry counter = %v, want 1", got)
	}
	if got := gaugeVecValue(t, observer.health.WithLabelValues("orders", "127.0.0.1:8080")); got != 1 {
		t.Fatalf("health gauge = %v, want 1", got)
	}

	observer.SetHealth("orders", "127.0.0.1:8080", false)
	if got := gaugeVecValue(t, observer.health.WithLabelValues("orders", "127.0.0.1:8080")); got != 0 {
		t.Fatalf("health gauge after unhealthy = %v, want 0", got)
	}

	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather private registry: %v", err)
	}
	if len(metrics) != 4 {
		t.Fatalf("registered metric families = %d, want 4", len(metrics))
	}
}

func TestProxyRuntimeObserverRejectsUnknownRetryResult(t *testing.T) {
	observer := newProxyRuntimeObserver(prometheus.NewRegistry())
	observer.ObserveRetry("orders", "connection reset")
	if got := counterVecValue(t, observer.retry.WithLabelValues("orders", "error")); got != 0 {
		t.Fatalf("retry counter for unknown result = %v, want 0", got)
	}
}

func TestNewProxyRuntimeObserverReturnsNonNil(t *testing.T) {
	observer := NewProxyRuntimeObserver()
	if observer == nil {
		t.Fatal("NewProxyRuntimeObserver() returned nil")
	}
}

func gaugeVecValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func counterVecValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}
