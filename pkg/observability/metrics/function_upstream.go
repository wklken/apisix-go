package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const functionUpstreamFailuresMetric = "function_upstream_failures_total"

var functionUpstreamFailures *prometheus.CounterVec

func newFunctionUpstreamFailureVector(registry *prometheus.Registry, prefix string) *prometheus.CounterVec {
	vector := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + functionUpstreamFailuresMetric,
		Help: "FaaS upstream failures by bounded plugin and reason",
	}, []string{"plugin", "reason"})
	if registry != nil {
		registry.MustRegister(vector)
	}
	return vector
}

func RecordFunctionUpstreamFailure(plugin string, reason string) bool {
	if functionUpstreamFailures == nil || !validFunctionUpstreamPlugin(plugin) ||
		!apisixctx.ValidResponseFailureReason(apisixctx.ResponseFailureReason(reason)) {
		return false
	}
	functionUpstreamFailures.WithLabelValues(plugin, reason).Inc()
	return true
}

func validFunctionUpstreamPlugin(plugin string) bool {
	switch plugin {
	case "function-upstream", "aws-lambda", "azure-functions", "openfunction":
		return true
	default:
		return false
	}
}
