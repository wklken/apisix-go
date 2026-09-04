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
	if len(metrics) != 5 {
		t.Fatalf("registered metric families = %d, want 5", len(metrics))
	}
}

func TestProxyRuntimeObserverRejectsUnknownRetryResult(t *testing.T) {
	observer := newProxyRuntimeObserver(prometheus.NewRegistry())
	observer.ObserveRetry("orders", "connection reset")
	if got := counterVecValue(t, observer.retry.WithLabelValues("orders", "error")); got != 0 {
		t.Fatalf("retry counter for unknown result = %v, want 0", got)
	}
}

func TestProxyRuntimeObserverBoundsProviderControlledLabels(t *testing.T) {
	observer := newProxyRuntimeObserver(prometheus.NewRegistry())
	observer.inFlightSeries = newMetricSeriesTracker(1, 1, 0, nil, observer.inFlight.DeleteLabelValues)
	observer.rejectedSeries = newMetricSeriesTracker(1, 1, 0, nil, observer.rejected.DeleteLabelValues)
	observer.retrySeries = newMetricSeriesTracker(1, 2, 0, nil, observer.retry.DeleteLabelValues)
	observer.healthSeries = newMetricSeriesTracker(1, 2, 0, nil, observer.health.DeleteLabelValues)

	observer.SetInFlight("orders", 1)
	observer.SetInFlight("payments", 1)
	observer.ObserveRejected("orders")
	observer.ObserveRejected("payments")
	observer.ObserveRetry("orders", "error")
	observer.ObserveRetry("payments", "error")
	observer.SetHealth("orders", "127.0.0.1:8080", true)
	observer.SetHealth("payments", "127.0.0.1:8081", true)

	for name, tracker := range map[string]*metricSeriesTracker{
		"in-flight": observer.inFlightSeries,
		"rejected":  observer.rejectedSeries,
		"retry":     observer.retrySeries,
		"health":    observer.healthSeries,
	} {
		if got := metricSeriesEntryCount(tracker); got != 1 {
			t.Fatalf("%s admitted series = %d, want 1", name, got)
		}
	}
	if got := gaugeVecValue(t, observer.inFlight.WithLabelValues(overflowLabel)); got != 1 {
		t.Fatalf("in-flight overflow = %v, want 1", got)
	}
	if got := counterVecValue(t, observer.rejected.WithLabelValues(overflowLabel)); got != 1 {
		t.Fatalf("rejected overflow = %v, want 1", got)
	}
	if got := counterVecValue(t, observer.retry.WithLabelValues(overflowLabel, overflowLabel)); got != 1 {
		t.Fatalf("retry overflow = %v, want 1", got)
	}
	if got := gaugeVecValue(t, observer.health.WithLabelValues(overflowLabel, overflowLabel)); got != 1 {
		t.Fatalf("health overflow = %v, want 1", got)
	}
}

func TestNewProxyRuntimeObserverReturnsNonNil(t *testing.T) {
	observer := NewProxyRuntimeObserver()
	if observer == nil {
		t.Fatal("NewProxyRuntimeObserver() returned nil")
	}
}

func TestProxyRuntimeObserverDeletesAllClusterSeries(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer := newProxyRuntimeObserver(registry)
	observer.SetInFlight("orders", 1)
	observer.ObserveRejected("orders")
	observer.ObserveRetry("orders", "success")
	observer.SetHealth("orders", "127.0.0.1:8080", true)

	observer.DeleteCluster("orders")
	metrics, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather private registry: %v", err)
	}
	if len(metrics) != 0 {
		t.Fatalf("metric families after cluster retirement = %d, want 0", len(metrics))
	}
}

func TestOfficialUpstreamStatusUsesTargetLabelsAndRetiresChildren(t *testing.T) {
	oldVector, oldTracker := UpstreamStatus, upstreamStatusSeries
	UpstreamStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_official_upstream_status"}, []string{"name", "ip", "port"},
	)
	upstreamStatusSeries = newMetricSeriesTracker(10, 3, 0, nil, UpstreamStatus.DeleteLabelValues)
	t.Cleanup(func() { UpstreamStatus, upstreamStatusSeries = oldVector, oldTracker })

	setUpstreamStatus("orders", "http://127.0.0.1:8080", true)
	if got := gaugeVecValue(t, UpstreamStatus.WithLabelValues("orders", "127.0.0.1", "8080")); got != 1 {
		t.Fatalf("official upstream status = %v, want 1", got)
	}
	upstreamStatusSeries.deleteMatching(func(labels []string) bool { return labels[0] == "orders" })
	if got := gaugeVecValue(t, UpstreamStatus.WithLabelValues("orders", "127.0.0.1", "8080")); got != 0 {
		t.Fatalf("retired official upstream status = %v, want 0", got)
	}
}

func TestProxyRuntimeObserverDeletesOneRetiredUpstreamTarget(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer := newProxyRuntimeObserver(registry)
	observer.SetHealth("orders", "http://127.0.0.1:8080", true)
	observer.SetHealth("orders", "http://127.0.0.1:8081", true)
	observer.DeleteUpstreamStatus("orders", "http://127.0.0.1:8080")

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 2 {
		t.Fatalf("upstream metric families after targeted delete = %#v, want health and status", families)
	}
	for _, family := range families {
		if len(family.Metric) != 1 {
			t.Fatalf("%s children after targeted delete = %#v, want one", family.GetName(), family.Metric)
		}
		labels := family.Metric[0].Label
		for _, label := range labels {
			if label.GetName() == "target" && label.GetValue() != "http://127.0.0.1:8081" {
				t.Fatalf("retained health target = %q, want replacement target", label.GetValue())
			}
			if label.GetName() == "port" && label.GetValue() != "8081" {
				t.Fatalf("retained status port = %q, want 8081", label.GetValue())
			}
		}
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
