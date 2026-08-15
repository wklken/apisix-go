package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordFunctionUpstreamFailureUsesBoundedLabels(t *testing.T) {
	previous := functionUpstreamFailures
	vector := newFunctionUpstreamFailureVector(prometheus.NewRegistry(), "test_")
	functionUpstreamFailures = vector
	t.Cleanup(func() { functionUpstreamFailures = previous })

	if !RecordFunctionUpstreamFailure("aws-lambda", "upstream_idle_timeout") ||
		!RecordFunctionUpstreamFailure("azure-functions", "upstream_copy_error") ||
		!RecordFunctionUpstreamFailure("openfunction", "client_canceled") {
		t.Fatal("valid function upstream failure was rejected")
	}
	if RecordFunctionUpstreamFailure("unknown", "upstream_idle_timeout") ||
		RecordFunctionUpstreamFailure("aws-lambda", "raw-error") {
		t.Fatal("unbounded function upstream metric label was accepted")
	}
	metric := &dto.Metric{}
	if err := vector.WithLabelValues("aws-lambda", "upstream_idle_timeout").Write(metric); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := metric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("idle timeout count = %v, want 1", got)
	}
}
