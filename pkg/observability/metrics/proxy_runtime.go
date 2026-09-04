package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/proxy/observer"
)

const (
	proxyInFlightMetric = "upstream_in_flight"
	proxyRejectedMetric = "upstream_rejected_total"
	proxyRetryMetric    = "upstream_retry_total"
	proxyHealthMetric   = "upstream_health"
)

// NewProxyRuntimeObserver returns a ClusterObserver that publishes bounded
// per-upstream cluster signals. The vectors are registered through the
// package's init-once lifecycle in Init. The observer resolves the current
// vectors lazily at call time, so constructing it before metrics.Init() is
// safe: every method is a no-op until Init registers the vectors.
func NewProxyRuntimeObserver() observer.ClusterObserver {
	return &proxyRuntimeObserver{}
}

// newProxyRuntimeObserver builds an observer against a private registry so
// tests can assert exact values without polluting the default registry.
func newProxyRuntimeObserver(registry *prometheus.Registry) *proxyRuntimeObserver {
	inFlight := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "apisix_" + proxyInFlightMetric, Help: "active upstream response bodies"},
		[]string{"upstream"},
	)
	rejected := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apisix_" + proxyRejectedMetric, Help: "overloaded upstream requests"},
		[]string{"upstream"},
	)
	retry := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "apisix_" + proxyRetryMetric, Help: "upstream transport outcomes"},
		[]string{"upstream", "result"},
	)
	health := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "apisix_" + proxyHealthMetric, Help: "upstream target health"},
		[]string{"upstream", "target"},
	)
	status := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "apisix_upstream_status", Help: "upstream target health"},
		[]string{"name", "ip", "port"},
	)
	registry.MustRegister(inFlight, rejected, retry, health, status)
	return &proxyRuntimeObserver{
		inFlight: inFlight, rejected: rejected, retry: retry, health: health, status: status,
		inFlightSeries: newMetricSeriesTracker(defaultMaxMetricSeries, 1, 0, nil, inFlight.DeleteLabelValues),
		rejectedSeries: newMetricSeriesTracker(defaultMaxMetricSeries, 1, 0, nil, rejected.DeleteLabelValues),
		retrySeries:    newMetricSeriesTracker(defaultMaxMetricSeries, 2, 0, nil, retry.DeleteLabelValues),
		healthSeries:   newMetricSeriesTracker(defaultMaxMetricSeries, 2, 0, nil, health.DeleteLabelValues),
		statusSeries:   newMetricSeriesTracker(defaultMaxMetricSeries, 3, 0, nil, status.DeleteLabelValues),
	}
}

type proxyRuntimeObserver struct {
	inFlight *prometheus.GaugeVec
	rejected *prometheus.CounterVec
	retry    *prometheus.CounterVec
	health   *prometheus.GaugeVec
	status   *prometheus.GaugeVec

	inFlightSeries *metricSeriesTracker
	rejectedSeries *metricSeriesTracker
	retrySeries    *metricSeriesTracker
	healthSeries   *metricSeriesTracker
	statusSeries   *metricSeriesTracker
}

func (o *proxyRuntimeObserver) SetInFlight(cluster string, delta int) {
	if vec := o.vector(ProxyInFlight, o.inFlight); vec != nil {
		o.tracker(proxyInFlightSeries, o.inFlightSeries).withSeries([]string{cluster}, func(labels []string) {
			vec.WithLabelValues(labels...).Add(float64(delta))
		})
	}
}

func (o *proxyRuntimeObserver) ObserveRejected(cluster string) {
	if vec := o.counter(ProxyRejected, o.rejected); vec != nil {
		o.tracker(proxyRejectedSeries, o.rejectedSeries).withSeries([]string{cluster}, func(labels []string) {
			vec.WithLabelValues(labels...).Inc()
		})
	}
}

func (o *proxyRuntimeObserver) ObserveRetry(cluster, result string) {
	if vec := o.counter(ProxyRetry, o.retry); vec != nil {
		switch result {
		case "success", "error", "stopped":
			o.tracker(proxyRetrySeries, o.retrySeries).withSeries([]string{cluster, result}, func(labels []string) {
				vec.WithLabelValues(labels...).Inc()
			})
		}
	}
}

func (o *proxyRuntimeObserver) SetHealth(cluster, target string, healthy bool) {
	if vec := o.vector(ProxyHealth, o.health); vec != nil {
		value := 0.0
		if healthy {
			value = 1
		}
		o.tracker(proxyHealthSeries, o.healthSeries).withSeries([]string{cluster, target}, func(labels []string) {
			vec.WithLabelValues(labels...).Set(value)
		})
	}
	if o.status != nil {
		ip, port := upstreamTargetLabels(target)
		value := 0.0
		if healthy {
			value = 1
		}
		o.statusSeries.withSeries([]string{cluster, ip, port}, func(labels []string) {
			o.status.WithLabelValues(labels...).Set(value)
		})
	} else {
		setUpstreamStatus(cluster, target, healthy)
	}
}

// SetUpstreamStatus publishes the official target-shaped health family. It is
// separate from SetHealth so registry initialization can expose configured
// targets before an active health checker emits a transition.
func (o *proxyRuntimeObserver) SetUpstreamStatus(cluster, target string, healthy bool) {
	if o.status != nil {
		ip, port := upstreamTargetLabels(target)
		value := 0.0
		if healthy {
			value = 1
		}
		o.statusSeries.withSeries([]string{cluster, ip, port}, func(labels []string) {
			o.status.WithLabelValues(labels...).Set(value)
		})
		return
	}
	setUpstreamStatus(cluster, target, healthy)
}

func (o *proxyRuntimeObserver) DeleteUpstreamStatus(cluster, target string) {
	if vec := o.vector(ProxyHealth, o.health); vec != nil {
		tracker := o.tracker(proxyHealthSeries, o.healthSeries)
		if tracker != nil {
			tracker.deleteMatching(func(labels []string) bool {
				return len(labels) == 2 && labels[0] == cluster && labels[1] == target
			})
		} else {
			vec.DeleteLabelValues(cluster, target)
		}
	}
	ip, port := upstreamTargetLabels(target)
	if o.status != nil {
		o.statusSeries.deleteMatching(func(labels []string) bool {
			return len(labels) == 3 && labels[0] == cluster && labels[1] == ip && labels[2] == port
		})
		return
	}
	if upstreamStatusSeries != nil {
		upstreamStatusSeries.deleteMatching(func(labels []string) bool {
			return len(labels) == 3 && labels[0] == cluster && labels[1] == ip && labels[2] == port
		})
		return
	}
	if UpstreamStatus != nil {
		UpstreamStatus.DeleteLabelValues(cluster, ip, port)
	}
}

func (o *proxyRuntimeObserver) DeleteCluster(cluster string) {
	labels := prometheus.Labels{"upstream": cluster}
	if vec := o.vector(ProxyInFlight, o.inFlight); vec != nil {
		deleteProxyClusterSeries(o.tracker(proxyInFlightSeries, o.inFlightSeries), cluster, func() {
			vec.DeletePartialMatch(labels)
		})
	}
	if vec := o.counter(ProxyRejected, o.rejected); vec != nil {
		deleteProxyClusterSeries(o.tracker(proxyRejectedSeries, o.rejectedSeries), cluster, func() {
			vec.DeletePartialMatch(labels)
		})
	}
	if vec := o.counter(ProxyRetry, o.retry); vec != nil {
		deleteProxyClusterSeries(o.tracker(proxyRetrySeries, o.retrySeries), cluster, func() {
			vec.DeletePartialMatch(labels)
		})
	}
	if vec := o.vector(ProxyHealth, o.health); vec != nil {
		deleteProxyClusterSeries(o.tracker(proxyHealthSeries, o.healthSeries), cluster, func() {
			vec.DeletePartialMatch(labels)
		})
	}
	if UpstreamStatus != nil {
		UpstreamStatus.DeletePartialMatch(prometheus.Labels{"name": cluster})
	}
	if upstreamStatusSeries != nil {
		upstreamStatusSeries.deleteMatching(func(labels []string) bool {
			return len(labels) > 0 && labels[0] == cluster
		})
	}
	if o.status != nil {
		deleteProxyClusterSeries(o.statusSeries, cluster, func() {
			o.status.DeletePartialMatch(prometheus.Labels{"name": cluster})
		})
	}
}

func (o *proxyRuntimeObserver) tracker(
	packageTracker *metricSeriesTracker,
	injected *metricSeriesTracker,
) *metricSeriesTracker {
	if injected != nil {
		return injected
	}
	return packageTracker
}

func deleteProxyClusterSeries(tracker *metricSeriesTracker, cluster string, fallback func()) {
	if tracker == nil {
		fallback()
		return
	}
	tracker.deleteMatching(func(labels []string) bool {
		return len(labels) > 0 && labels[0] == cluster
	})
}

func (o *proxyRuntimeObserver) vector(
	packageVec *prometheus.GaugeVec,
	injected *prometheus.GaugeVec,
) *prometheus.GaugeVec {
	if injected != nil {
		return injected
	}
	return packageVec
}

func (o *proxyRuntimeObserver) counter(
	packageVec *prometheus.CounterVec,
	injected *prometheus.CounterVec,
) *prometheus.CounterVec {
	if injected != nil {
		return injected
	}
	return packageVec
}
