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
// endpoints. ConfigApplyReady requires the provider and HTTP route stages,
// plus the stream stage when stream publication is required, to have been
// observed successful; EtcdReachable is the current etcd observation.
type ReadinessState struct {
	ConfigApplyReady bool
	EtcdReachable    bool
}

// ConfigApplyStage identifies one bounded configuration-apply owner. Readiness
// is healthy only when the required stages have been observed successful and
// none is currently unhealthy.
type ConfigApplyStage uint8

const (
	ConfigApplyStageProvider ConfigApplyStage = iota
	ConfigApplyStageHTTPRoutes
	ConfigApplyStageStreams
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
	streamRequired     bool
	streamBlocked      bool
	providerObserved   bool
	providerHealthy    bool
	httpRoutesObserved bool
	httpRoutesHealthy  bool
	streamObserved     bool
	streamHealthy      bool
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
		Help: "Whether required provider/store, HTTP route, and stream publication stages are healthy",
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
	case ConfigApplyStageStreams:
		configApplyState.streamBlocked = true
		configApplyState.streamObserved = true
		configApplyState.streamHealthy = false
	default:
		return
	}
	if failures != nil {
		failures.Inc()
	}
	setConfigApplyReadyLocked(ready)
}

// RecordConfigApplyAttemptFailure records a failed provider attempt without
// replacing the readiness state established by the last acknowledged apply.
// Provider and stage are intentionally bounded call-site metadata rather than
// metric labels; the exported counter keeps fixed cardinality.
func RecordConfigApplyAttemptFailure(provider, stage string) {
	_, _ = provider, stage
	configApplyState.Lock()
	defer configApplyState.Unlock()

	failures, _ := syncConfigApplyMetricsLocked()
	if failures != nil {
		failures.Inc()
	}
}

// RecordConfigApplyAcknowledgement installs provider, publication-stage, and
// quarantine readiness from one durable acknowledgement while holding the
// readiness state lock once. Domains not represented by the acknowledgement
// retain their prior state.
func RecordConfigApplyAcknowledgement(httpApplied, streamApplied bool, quarantine int) {
	if quarantine < 0 {
		quarantine = 0
	}
	configApplyState.Lock()
	defer configApplyState.Unlock()

	_, ready := syncConfigApplyMetricsLocked()
	configApplyState.providerBlocked = false
	configApplyState.providerObserved = true
	configApplyState.providerHealthy = true
	if httpApplied {
		configApplyState.httpRoutesBlocked = false
		configApplyState.httpRoutesObserved = true
		configApplyState.httpRoutesHealthy = true
	}
	if streamApplied {
		configApplyState.streamBlocked = false
		configApplyState.streamObserved = true
		configApplyState.streamHealthy = true
	}
	configApplyState.providerQuarantine = quarantine
	configApplyState.quarantineCount = configApplyState.providerQuarantine + configApplyState.storeQuarantine
	if configApplyState.quarantined != nil {
		configApplyState.quarantined.Set(float64(configApplyState.quarantineCount))
	}
	setConfigApplyReadyLocked(ready)
}

// RecordConfigApplyStageSuccess clears only the supplied stage's blocker and
// marks readiness healthy when every required stage is also healthy. It is
// safe before metrics.Init().
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
	case ConfigApplyStageStreams:
		configApplyState.streamBlocked = false
		configApplyState.streamObserved = true
		configApplyState.streamHealthy = true
	default:
		return
	}
	setConfigApplyReadyLocked(ready)
}

// SetConfigApplyStreamRequired controls whether stream publication participates
// in config-apply readiness. Changing the requirement clears the stream
// observation so enabling it always requires a fresh successful publication.
func SetConfigApplyStreamRequired(required bool) {
	configApplyState.Lock()
	defer configApplyState.Unlock()

	_, ready := syncConfigApplyMetricsLocked()
	if configApplyState.streamRequired != required {
		configApplyState.streamRequired = required
		configApplyState.streamBlocked = false
		configApplyState.streamObserved = false
		configApplyState.streamHealthy = false
	}
	setConfigApplyReadyLocked(ready)
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
	setConfigApplyReadyLocked(ready)
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
		ConfigApplyReady: configApplyReadyLocked(),
		EtcdReachable:    configApplyState.etcdObserved && configApplyState.etcdHealthy,
	}
}

func syncConfigApplyMetricsLocked() (prometheus.Counter, prometheus.Gauge) {
	failures, ready := ConfigApplyFailures, ConfigApplyReady
	quarantined := ConfigApplyQuarantined
	if configApplyState.failures != failures || configApplyState.ready != ready ||
		configApplyState.quarantined != quarantined {
		configApplyState.providerBlocked = false
		configApplyState.httpRoutesBlocked = false
		configApplyState.streamRequired = false
		configApplyState.streamBlocked = false
		configApplyState.providerObserved = false
		configApplyState.providerHealthy = false
		configApplyState.httpRoutesObserved = false
		configApplyState.httpRoutesHealthy = false
		configApplyState.streamObserved = false
		configApplyState.streamHealthy = false
		configApplyState.providerQuarantine = 0
		configApplyState.storeQuarantine = 0
		configApplyState.quarantineCount = 0
		configApplyState.failures = failures
		configApplyState.ready = ready
		configApplyState.quarantined = quarantined
	}
	return failures, ready
}

func configApplyReadyLocked() bool {
	if configApplyState.quarantineCount != 0 ||
		configApplyState.providerBlocked || !configApplyState.providerObserved ||
		!configApplyState.providerHealthy || configApplyState.httpRoutesBlocked ||
		!configApplyState.httpRoutesObserved || !configApplyState.httpRoutesHealthy {
		return false
	}
	if configApplyState.streamRequired &&
		(configApplyState.streamBlocked || !configApplyState.streamObserved ||
			!configApplyState.streamHealthy) {
		return false
	}
	return true
}

func setConfigApplyReadyLocked(ready prometheus.Gauge) {
	if ready == nil {
		return
	}
	if configApplyReadyLocked() {
		ready.Set(1)
		return
	}
	ready.Set(0)
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
