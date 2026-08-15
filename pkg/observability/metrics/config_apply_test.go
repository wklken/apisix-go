package metrics

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestConfigApplyMetricsAreNilSafeBeforeInit(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures, ConfigApplyReady = nil, nil
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyFailure()
	RecordConfigApplySuccess()
	RecordConfigApplyStageFailure(ConfigApplyStageHTTPRoutes)
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
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

func TestConfigApplyStagesKeepReadinessBlockedIndependently(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_config_apply_stage_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_stage_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageFailure(ConfigApplyStageHTTPRoutes)
	RecordConfigApplySuccess()
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider success = %v, want 0", got)
	}
	if got := counterValue(t, ConfigApplyFailures); got != 1 {
		t.Fatalf("failure count after HTTP stage failure = %v, want 1", got)
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after both stages recover = %v, want 1", got)
	}

	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after provider failure and HTTP success = %v, want 0", got)
	}
	if got := counterValue(t, ConfigApplyFailures); got != 2 {
		t.Fatalf("failure count after provider stage failure = %v, want 2", got)
	}
}

func TestConfigApplyStagesAreConcurrencySafe(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_config_apply_stage_concurrent_failures_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_config_apply_stage_concurrent_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	const calls = 64
	var group sync.WaitGroup
	group.Add(calls)
	for index := range calls {
		go func(index int) {
			defer group.Done()
			stage := ConfigApplyStageProvider
			if index%2 == 0 {
				stage = ConfigApplyStageHTTPRoutes
			}
			RecordConfigApplyStageFailure(stage)
		}(index)
	}
	group.Wait()

	if got := counterValue(t, ConfigApplyFailures); got != calls {
		t.Fatalf("concurrent failure count = %v, want %d", got, calls)
	}
	if got := gaugeValue(t, ConfigApplyReady); got != 0 {
		t.Fatalf("ready after concurrent failures = %v, want 0", got)
	}

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := gaugeValue(t, ConfigApplyReady); got != 1 {
		t.Fatalf("ready after concurrent stage recovery = %v, want 1", got)
	}
}

func TestGetReadinessRequiresBothConfigApplyStages(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_readiness_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true before either stage was observed")
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true before HTTP route stage was observed")
	}
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false after both stages succeeded")
	}

	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after provider stage failed")
	}
}

func TestGetReadinessResetsWhenConfigMetricsAreReplaced(t *testing.T) {
	oldFailures, oldReady := ConfigApplyFailures, ConfigApplyReady
	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_readiness_reset_failures_total"})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_reset_ready"})
	t.Cleanup(func() { ConfigApplyFailures, ConfigApplyReady = oldFailures, oldReady })

	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
	RecordConfigApplyStageSuccess(ConfigApplyStageHTTPRoutes)
	if got := GetReadiness().ConfigApplyReady; !got {
		t.Fatal("config apply readiness = false before collector replacement")
	}

	ConfigApplyFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_readiness_reset_failures_replaced_total",
	})
	ConfigApplyReady = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_reset_ready_replaced"})
	if got := GetReadiness().ConfigApplyReady; got {
		t.Fatal("config apply readiness = true after collector replacement")
	}
}
