package metrics

import "github.com/prometheus/client_golang/prometheus"

const aiSafetyOutcomesMetric = "ai_safety_outcomes_total"

func newAISafetyOutcomeVector(registry *prometheus.Registry, prefix string) *prometheus.CounterVec {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prefix + aiSafetyOutcomesMetric,
			Help: "AI safety outcomes by plugin, phase, outcome, and reason",
		},
		[]string{"plugin", "phase", "outcome", "reason"},
	)
	if registry != nil {
		registry.MustRegister(vector)
	}
	return vector
}

// RecordAISafetyOutcome increments a fixed-label AI safety counter. It is a
// no-op before metrics initialization and rejects unknown labels so callers
// cannot create unbounded series.
func RecordAISafetyOutcome(plugin, phase, outcome, reason string) bool {
	vector := AISafetyOutcomes
	if vector == nil || !validAISafetyPlugin(plugin) || !validAISafetyPhase(phase) ||
		!validAISafetyOutcome(outcome) || !validAISafetyReason(reason) {
		return false
	}
	vector.WithLabelValues(plugin, phase, outcome, reason).Inc()
	return true
}

func validAISafetyPlugin(plugin string) bool {
	switch plugin {
	case "ai-prompt-guard", "ai-aliyun-content-moderation":
		return true
	default:
		return false
	}
}

func validAISafetyPhase(phase string) bool {
	return phase == "request" || phase == "response"
}

func validAISafetyOutcome(outcome string) bool {
	switch outcome {
	case "allow", "deny", "degraded", "error":
		return true
	default:
		return false
	}
}

func validAISafetyReason(reason string) bool {
	switch reason {
	case "invalid_payload", "unknown_protocol", "empty_content",
		"backend_unavailable", "backend_invalid_response", "upstream_invalid_response",
		"clean", "allow_pattern_miss", "deny_pattern_match", "risk_threshold":
		return true
	default:
		return false
	}
}
