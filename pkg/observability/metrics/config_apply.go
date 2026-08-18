package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	configApplyFailuresMetric    = "config_apply_failures_total"
	configApplyReadyMetric       = "config_apply_ready"
	configApplyQuarantinedMetric = "config_apply_quarantined_resources"
)

// ReadinessState contains the bounded runtime state used by the HTTP health
// endpoints. ConfigApplyReady requires both config-apply stages to have been
// observed successful; EtcdReachable is the current etcd observation.
type ReadinessState struct {
	ConfigApplyReady bool
	EtcdReachable    bool
}

// ConfigApplyStage identifies one bounded configuration-apply owner. Readiness
// is healthy only when both stages have been observed successful and neither
// stage is currently unhealthy.
type ConfigApplyStage uint8

const (
	ConfigApplyStageProvider ConfigApplyStage = iota
	ConfigApplyStageHTTPRoutes
)

var (
	ConfigApplyFailures    prometheus.Counter
	ConfigApplyReady       prometheus.Gauge
	ConfigApplyQuarantined prometheus.Gauge
)

var configApplyState struct {
	sync.Mutex
	providerBlocked    bool
	httpRoutesBlocked  bool
	providerObserved   bool
	providerHealthy    bool
	httpRoutesObserved bool
	httpRoutesHealthy  bool
	etcdObserved       bool
	etcdHealthy        bool
	failures           prometheus.Counter
	ready              prometheus.Gauge
	etcdReachable      prometheus.Gauge
	quarantined        prometheus.Gauge
	providerQuarantine int
	storeQuarantine    int
	quarantineCount    int
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

func newConfigApplyQuarantineMetric(registry *prometheus.Registry, prefix string) prometheus.Gauge {
	quarantined := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: prefix + configApplyQuarantinedMetric,
		Help: "Number of invalid configuration resources currently quarantined",
	})
	if registry != nil {
		registry.MustRegister(quarantined)
	}
	return quarantined
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
		configApplyState.providerObserved = true
		configApplyState.providerHealthy = false
	case ConfigApplyStageHTTPRoutes:
		configApplyState.httpRoutesBlocked = true
		configApplyState.httpRoutesObserved = true
		configApplyState.httpRoutesHealthy = false
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
		configApplyState.providerObserved = true
		configApplyState.providerHealthy = true
	case ConfigApplyStageHTTPRoutes:
		configApplyState.httpRoutesBlocked = false
		configApplyState.httpRoutesObserved = true
		configApplyState.httpRoutesHealthy = true
	default:
		return
	}
	if ready != nil && configApplyState.quarantineCount == 0 &&
		!configApplyState.providerBlocked && !configApplyState.httpRoutesBlocked {
		ready.Set(1)
	}
}

// RecordConfigApplyQuarantine updates the provider-side count of invalid
// resources retained at their last-good state. The count is deliberately
// exported as a no-label gauge so arbitrary etcd keys cannot create metric
// series. Store-side legacy quarantine is tracked independently through
// RecordConfigApplyStoreQuarantine.
func RecordConfigApplyQuarantine(count int) {
	configApplyState.Lock()
	defer configApplyState.Unlock()
	recordConfigApplyQuarantineLocked(&configApplyState.providerQuarantine, count)
}

// RecordConfigApplyStoreQuarantine updates the store-side count of malformed
// legacy route/global-rule rows skipped from a published snapshot. It is kept
// separate from the provider count so one source cannot clear the other.
func RecordConfigApplyStoreQuarantine(count int) {
	if count < 0 {
		count = 0
	}
	configApplyState.Lock()
	defer configApplyState.Unlock()
	recordConfigApplyQuarantineLocked(&configApplyState.storeQuarantine, count)
}

func recordConfigApplyQuarantineLocked(source *int, count int) {
	if count < 0 {
		count = 0
	}
	_, ready := syncConfigApplyMetricsLocked()
	*source = count
	configApplyState.quarantineCount = configApplyState.providerQuarantine + configApplyState.storeQuarantine
	if configApplyState.quarantined != nil {
		configApplyState.quarantined.Set(float64(configApplyState.quarantineCount))
	}
	if ready == nil {
		return
	}
	if configApplyState.quarantineCount > 0 {
		ready.Set(0)
		return
	}
	if configApplyState.providerObserved && configApplyState.providerHealthy &&
		configApplyState.httpRoutesObserved && configApplyState.httpRoutesHealthy &&
		!configApplyState.providerBlocked && !configApplyState.httpRoutesBlocked {
		ready.Set(1)
	}
}

// RecordConfigApplySuccess preserves the legacy provider/store stage API.
func RecordConfigApplySuccess() {
	RecordConfigApplyStageSuccess(ConfigApplyStageProvider)
}

// GetReadiness returns the internal observed/healthy state for health
// endpoints. It deliberately does not read values back from Prometheus
// collectors, so replacing a collector resets the corresponding state.
func GetReadiness() ReadinessState {
	configApplyState.Lock()
	defer configApplyState.Unlock()

	syncConfigApplyMetricsLocked()
	syncEtcdReachabilityLocked()
	return ReadinessState{
		ConfigApplyReady: configApplyState.providerObserved &&
			configApplyState.providerHealthy &&
			configApplyState.httpRoutesObserved &&
			configApplyState.httpRoutesHealthy &&
			configApplyState.quarantineCount == 0,
		EtcdReachable: configApplyState.etcdObserved && configApplyState.etcdHealthy,
	}
}

func syncConfigApplyMetricsLocked() (prometheus.Counter, prometheus.Gauge) {
	failures, ready := ConfigApplyFailures, ConfigApplyReady
	quarantined := ConfigApplyQuarantined
	if configApplyState.failures != failures || configApplyState.ready != ready ||
		configApplyState.quarantined != quarantined {
		configApplyState.providerBlocked = false
		configApplyState.httpRoutesBlocked = false
		configApplyState.providerObserved = false
		configApplyState.providerHealthy = false
		configApplyState.httpRoutesObserved = false
		configApplyState.httpRoutesHealthy = false
		configApplyState.providerQuarantine = 0
		configApplyState.storeQuarantine = 0
		configApplyState.quarantineCount = 0
		configApplyState.failures = failures
		configApplyState.ready = ready
		configApplyState.quarantined = quarantined
	}
	return failures, ready
}

func syncEtcdReachabilityLocked() prometheus.Gauge {
	reachable := EtcdReachable
	if configApplyState.etcdReachable != reachable {
		configApplyState.etcdObserved = false
		configApplyState.etcdHealthy = false
		configApplyState.etcdReachable = reachable
	}
	return reachable
}
