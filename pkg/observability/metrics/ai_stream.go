package metrics

import "github.com/prometheus/client_golang/prometheus"

const aiStreamOutcomesMetric = "ai_stream_outcomes_total"

var aiStreamOutcomes *prometheus.CounterVec

func newAIStreamOutcomeVector(registry *prometheus.Registry, prefix string) *prometheus.CounterVec {
	vector := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + aiStreamOutcomesMetric,
		Help: "Terminal AI streaming outcomes by bounded transport and result",
	}, []string{"transport", "outcome"})
	if registry != nil {
		registry.MustRegister(vector)
	}
	return vector
}

func RecordAIStreamOutcome(transport, outcome string) bool {
	if aiStreamOutcomes == nil || !validAIStreamTransport(transport) || !validAIStreamOutcome(outcome) {
		return false
	}
	aiStreamOutcomes.WithLabelValues(transport, outcome).Inc()
	return true
}

func validAIStreamTransport(transport string) bool {
	return transport == "sse" || transport == "aws_eventstream"
}

func validAIStreamOutcome(outcome string) bool {
	return outcome == "success" || outcome == "error" || outcome == "canceled"
}
