package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRecordAISafetyOutcomeBeforeInitIsNoOp(t *testing.T) {
	previous := AISafetyOutcomes
	AISafetyOutcomes = nil
	t.Cleanup(func() { AISafetyOutcomes = previous })

	if got := RecordAISafetyOutcome("ai-prompt-guard", "request", "allow", "clean"); got {
		t.Fatal("RecordAISafetyOutcome() = true before Init(), want false")
	}
}

func TestAISafetyRecordAISafetyOutcomeUsesPrivateRegistryAndBoundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	vector := newAISafetyOutcomeVector(registry, "test_")
	previous := AISafetyOutcomes
	AISafetyOutcomes = vector
	t.Cleanup(func() { AISafetyOutcomes = previous })

	if !RecordAISafetyOutcome("ai-prompt-guard", "request", "allow", "clean") {
		t.Fatal("valid prompt-guard outcome returned false")
	}
	if !RecordAISafetyOutcome("ai-aliyun-content-moderation", "response", "error", "backend_unavailable") {
		t.Fatal("valid Aliyun outcome returned false")
	}
	for _, labels := range [][4]string{
		{"unknown-plugin", "request", "allow", "clean"},
		{"ai-prompt-guard", "stream", "allow", "clean"},
		{"ai-prompt-guard", "request", "unknown", "clean"},
		{"ai-prompt-guard", "request", "allow", "unknown"},
	} {
		if RecordAISafetyOutcome(labels[0], labels[1], labels[2], labels[3]) {
			t.Fatalf("unknown labels %v returned true", labels)
		}
	}

	metric := vector.WithLabelValues("ai-prompt-guard", "request", "allow", "clean")
	if got := counterValue(t, metric); got != 1 {
		t.Fatalf("prompt-guard allow count = %v, want 1", got)
	}
	metric = vector.WithLabelValues("ai-aliyun-content-moderation", "response", "error", "backend_unavailable")
	if got := counterValue(t, metric); got != 1 {
		t.Fatalf("Aliyun error count = %v, want 1", got)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("private registry gather: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "test_ai_safety_outcomes_total" {
		t.Fatalf("gathered metric families = %#v, want private AI safety family", families)
	}
}

func TestAISafetyOutcomeAcceptsAllFixedReasons(t *testing.T) {
	vector := newAISafetyOutcomeVector(prometheus.NewRegistry(), "reasons_")
	previous := AISafetyOutcomes
	AISafetyOutcomes = vector
	t.Cleanup(func() { AISafetyOutcomes = previous })

	reasons := []string{
		"invalid_payload",
		"unknown_protocol",
		"empty_content",
		"backend_unavailable",
		"backend_invalid_response",
		"upstream_invalid_response",
		"clean",
		"allow_pattern_miss",
		"deny_pattern_match",
		"risk_threshold",
	}
	for _, reason := range reasons {
		if !RecordAISafetyOutcome("ai-prompt-guard", "response", "degraded", reason) {
			t.Errorf("fixed reason %q returned false", reason)
		}
	}
}
