package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordAIStreamOutcomeUsesPrivateBoundedLabels(t *testing.T) {
	previous := aiStreamOutcomes
	vector := newAIStreamOutcomeVector(prometheus.NewRegistry(), "test_")
	aiStreamOutcomes = vector
	t.Cleanup(func() { aiStreamOutcomes = previous })

	if !RecordAIStreamOutcome("sse", "success") ||
		!RecordAIStreamOutcome("aws_eventstream", "error") ||
		!RecordAIStreamOutcome("sse", "canceled") {
		t.Fatal("valid AI stream outcome was rejected")
	}
	if RecordAIStreamOutcome("unknown", "success") || RecordAIStreamOutcome("sse", "other") {
		t.Fatal("unbounded AI stream label was accepted")
	}
	metric := &dto.Metric{}
	if err := vector.WithLabelValues("sse", "success").Write(metric); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("SSE success count = %v, want 1", got)
	}
}
