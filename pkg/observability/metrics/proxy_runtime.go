package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/proxy"
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
func NewProxyRuntimeObserver() proxy.ClusterObserver {
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
	registry.MustRegister(inFlight, rejected, retry, health)
	return &proxyRuntimeObserver{inFlight: inFlight, rejected: rejected, retry: retry, health: health}
}

type proxyRuntimeObserver struct {
	inFlight *prometheus.GaugeVec
	rejected *prometheus.CounterVec
	retry    *prometheus.CounterVec
	health   *prometheus.GaugeVec
}

func (o *proxyRuntimeObserver) SetInFlight(cluster string, delta int) {
	if vec := o.vector(ProxyInFlight, o.inFlight); vec != nil {
		vec.WithLabelValues(cluster).Add(float64(delta))
	}
}

func (o *proxyRuntimeObserver) ObserveRejected(cluster string) {
	if vec := o.counter(ProxyRejected, o.rejected); vec != nil {
		vec.WithLabelValues(cluster).Inc()
	}
}

func (o *proxyRuntimeObserver) ObserveRetry(cluster, result string) {
	if vec := o.counter(ProxyRetry, o.retry); vec != nil {
		switch result {
		case "success", "error", "stopped":
			vec.WithLabelValues(cluster, result).Inc()
		}
	}
}

func (o *proxyRuntimeObserver) SetHealth(cluster, target string, healthy bool) {
	if vec := o.vector(ProxyHealth, o.health); vec != nil {
		value := 0.0
		if healthy {
			value = 1
		}
		vec.WithLabelValues(cluster, target).Set(value)
	}
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
