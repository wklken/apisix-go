package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordRequestPanicIsNilSafeBeforeInit(t *testing.T) {
	previous := requestPanics
	requestPanics = nil
	t.Cleanup(func() { requestPanics = previous })

	for _, stage := range []RequestPanicStage{
		RequestPanicPreCommit,
		RequestPanicPostCommit,
		RequestPanicPostFlush,
		RequestPanicPostHijack,
		RequestPanicFinalizer,
	} {
		RecordRequestPanic(stage)
	}
	RecordRequestPanic(RequestPanicStage("unbounded-value"))
}

func TestRequestPanicMetricUsesOnlyBoundedStages(t *testing.T) {
	registry := prometheus.NewRegistry()
	metric := newRequestPanicMetrics(registry, "apisix_")
	previous := requestPanics
	requestPanics = metric
	t.Cleanup(func() { requestPanics = previous })

	stages := []RequestPanicStage{
		RequestPanicPreCommit,
		RequestPanicPostCommit,
		RequestPanicPostFlush,
		RequestPanicPostHijack,
		RequestPanicFinalizer,
	}
	for _, stage := range stages {
		RecordRequestPanic(stage)
	}
	RecordRequestPanic(RequestPanicStage("panic-value"))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "apisix_http_request_panics_total" {
		t.Fatalf("metric families = %#v", families)
	}
	if got := len(families[0].GetMetric()); got != len(stages) {
		t.Fatalf("stage series = %d, want %d", got, len(stages))
	}
	for _, sample := range families[0].GetMetric() {
		labels := sample.GetLabel()
		if len(labels) != 1 || labels[0].GetName() != "stage" {
			t.Fatalf("labels = %v, want only stage", labels)
		}
		if sample.GetCounter().GetValue() != 1 {
			t.Fatalf("stage %q count = %v, want 1", labels[0].GetValue(), sample.GetCounter().GetValue())
		}
	}
}

func TestRequestPanicMetricRegistrationAndFacade(t *testing.T) {
	registry := prometheus.NewRegistry()
	metric := newRequestPanicMetrics(registry, "test_")
	previous := requestPanics
	requestPanics = metric
	t.Cleanup(func() { requestPanics = previous })

	RecordRequestPanic(RequestPanicPreCommit)
	collected, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(collected) != 1 || len(collected[0].GetMetric()) != 1 {
		t.Fatalf("collected = %#v", collected)
	}
	value := &dto.Metric{}
	if err := metric.WithLabelValues(string(RequestPanicPreCommit)).Write(value); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := value.GetCounter().GetValue(); got != 1 {
		t.Fatalf("pre-commit count = %v, want 1", got)
	}
}
