package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestConfigApplyMetricsAreNilSafeBeforeInit(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures, ConfigApplyReady = nil, nil
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyFailure()
	RecordConfigApplySuccess()
}

func TestConfigApplyMetricsUseFixedNoLabelCardinality(t *testing.T) {
	registry := prometheus.NewRegistry()
	failures, ready := newConfigApplyMetrics(registry, "apisix_")
	failures.Inc()
	ready.Set(1)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 2 {
		t.Fatalf("metric families = %d, want 2", len(families))
	}
	for _, family := range families {
		if got := len(family.GetMetric()); got != 1 {
			t.Fatalf("%s series = %d, want 1", family.GetName(), got)
		}
		if labels := family.GetMetric()[0].GetLabel(); len(labels) != 0 {
			t.Fatalf("%s labels = %v, want none", family.GetName(), labels)
		}
	}
}

func TestRecordConfigApplyUpdatesFailureAndReady(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_config_apply_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplySuccess()
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after success = %v, want 1", got)
	}
	RecordConfigApplyFailure()
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count = %v, want 1", got)
	}
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after failure = %v, want 0", got)
	}
}
