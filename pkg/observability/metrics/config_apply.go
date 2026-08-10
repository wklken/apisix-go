package metrics

import "github.com/prometheus/client_golang/prometheus"

const (
	configApplyFailuresMetric = "config_apply_failures_total"
	configApplyReadyMetric    = "config_apply_ready"
)

var (
	ConfigApplyFailures prometheus.Counter
	ConfigApplyReady    prometheus.Gauge
)

func newConfigApplyMetrics(registry *prometheus.Registry, prefix string) (prometheus.Counter, prometheus.Gauge) {
	failures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: prefix + configApplyFailuresMetric,
		Help: "Total configuration apply failures",
	})
	ready := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: prefix + configApplyReadyMetric,
		Help: "Whether the most recent configuration apply completed successfully",
	})
	if registry != nil {
		registry.MustRegister(failures, ready)
	}
	return failures, ready
}

// RecordConfigApplyFailure marks the current configuration as unhealthy and
// increments the bounded failure counter. It is safe before metrics.Init().
func RecordConfigApplyFailure() {
	if ConfigApplyFailures != nil {
		ConfigApplyFailures.Inc()
	}
	if ConfigApplyReady != nil {
		ConfigApplyReady.Set(0)
	}
}

// RecordConfigApplySuccess marks the current configuration as ready. It is
// safe before metrics.Init().
func RecordConfigApplySuccess() {
	if ConfigApplyReady != nil {
		ConfigApplyReady.Set(1)
	}
}
