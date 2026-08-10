package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	configApplyFailuresMetric = "config_apply_failures_total"
	configApplyReadyMetric    = "config_apply_ready"
)

// ConfigApplyStage identifies one bounded configuration-apply owner. Readiness
// is healthy only when neither stage has a recorded failure.
type ConfigApplyStage uint8

const (
	ConfigApplyStageProvider ConfigApplyStage = iota
	ConfigApplyStageHTTPRoutes
)

var (
	ConfigApplyFailures prometheus.Counter
	ConfigApplyReady    prometheus.Gauge
)

var configApplyState struct {
	sync.Mutex
	providerBlocked   bool
	httpRoutesBlocked bool
	failures          prometheus.Counter
	ready             prometheus.Gauge
}

func newConfigApplyMetrics(registry *prometheus.Registry, prefix string) (prometheus.Counter, prometheus.Gauge) {
	failures := prometheus.NewCounter(prometheus.CounterOpts{
		Name: prefix + configApplyFailuresMetric,
		Help: "Total configuration apply failures",
	})
	ready := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: prefix + configApplyReadyMetric,
		Help: "Whether provider/store and HTTP route publication stages are healthy",
	})
	if registry != nil {
		registry.MustRegister(failures, ready)
	}
	return failures, ready
}

// RecordConfigApplyStageFailure marks one configuration-apply stage as
// unhealthy and increments the shared bounded failure counter once. It is
// safe before metrics.Init().
func RecordConfigApplyStageFailure(stage ConfigApplyStage) {
	configApplyState.Lock()
	defer configApplyState.Unlock()

	failures, ready := syncConfigApplyMetricsLocked()
	switch stage {
	case ConfigApplyStageProvider:
		configApplyState.providerBlocked = true
	case ConfigApplyStageHTTPRoutes:
		configApplyState.httpRoutesBlocked = true
	default:
		return
	}
	if failures != nil {
		failures.Inc()
	}
	if ready != nil {
		ready.Set(0)
	}
}

// RecordConfigApplyFailure preserves the legacy provider/store stage API.
func RecordConfigApplyFailure() {
	RecordConfigApplyStageFailure(ConfigApplyStageProvider)
}

// RecordConfigApplyStageSuccess clears only the supplied stage's blocker and
// marks readiness healthy when the other stage is also unblocked. It is safe
// before metrics.Init().
func RecordConfigApplyStageSuccess(stage ConfigApplyStage) {
	configApplyState.Lock()
	defer configApplyState.Unlock()

	_, ready := syncConfigApplyMetricsLocked()
	switch stage {
	case ConfigApplyStageProvider:
		configApplyState.providerBlocked = false
	case ConfigApplyStageHTTPRoutes:
		configApplyState.httpRoutesBlocked = false
	default:
		return
	}
	if ready != nil && !configApplyState.providerBlocked && !configApplyState.httpRoutesBlocked {
		ready.Set(1)
	}
}

// RecordConfigApplySuccess preserves the legacy provider/store stage API.
func RecordConfigApplySuccess() {
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
}

func syncConfigApplyMetricsLocked() (prometheus.Counter, prometheus.Gauge) {
	failures, ready := ConfigApplyFailures, ConfigApplyReady
	if configApplyState.failures != failures || configApplyState.ready != ready {
		configApplyState.providerBlocked = false
		configApplyState.httpRoutesBlocked = false
		configApplyState.failures = failures
		configApplyState.ready = ready
	}
	return failures, ready
}
