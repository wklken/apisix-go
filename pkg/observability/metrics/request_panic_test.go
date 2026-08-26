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

	for _, owner := range []RequestPanicOwner{
		RequestPanicPlugin,
		RequestPanicCore,
		RequestPanicPluginFinalizer,
		RequestPanicCoreFinalizer,
	} {
		RecordRequestPanic(owner, RequestPanicPreCommit)
	}
	RecordRequestPanic(RequestPanicOwner("unbounded-owner"), RequestPanicPreCommit)
	RecordRequestPanic(RequestPanicPlugin, RequestPanicStage("unbounded-stage"))
}

func TestRequestPanicMetricUsesOnlyBoundedOwnersAndStages(t *testing.T) {
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
	owners := []RequestPanicOwner{
		RequestPanicPlugin,
		RequestPanicCore,
		RequestPanicPluginFinalizer,
		RequestPanicCoreFinalizer,
	}
	for _, owner := range owners {
		for _, stage := range stages {
			RecordRequestPanic(owner, stage)
		}
	}
	RecordRequestPanic(RequestPanicOwner("factory-name"), RequestPanicPreCommit)
	RecordRequestPanic(RequestPanicPlugin, RequestPanicStage("raw panic text"))

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "apisix_http_request_panics_total" {
		t.Fatalf("metric families = %#v", families)
	}
	if got, want := len(families[0].GetMetric()), len(owners)*len(stages); got != want {
		t.Fatalf("owner/stage series = %d, want %d", got, want)
	}
	for _, sample := range families[0].GetMetric() {
		labels := sample.GetLabel()
		if len(labels) != 2 || labels[0].GetName() != "owner" || labels[1].GetName() != "stage" {
			t.Fatalf("labels = %v, want owner and stage", labels)
		}
		if sample.GetCounter().GetValue() != 1 {
			t.Fatalf("owner/stage %q/%q count = %v, want 1",
				labels[0].GetValue(), labels[1].GetValue(), sample.GetCounter().GetValue())
		}
		if labels[0].GetValue() == "factory-name" || labels[1].GetValue() == "raw panic text" {
			t.Fatalf("unbounded label escaped validation: %v", labels)
		}
	}
}

func TestRequestPanicMetricRegistrationAndFacade(t *testing.T) {
	registry := prometheus.NewRegistry()
	metric := newRequestPanicMetrics(registry, "test_")
	previous := requestPanics
	requestPanics = metric
	t.Cleanup(func() { requestPanics = previous })

	RecordRequestPanic(RequestPanicPlugin, RequestPanicPreCommit)
	collected, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(collected) != 1 || len(collected[0].GetMetric()) != 1 {
		t.Fatalf("collected = %#v", collected)
	}
	value := &dto.Metric{}
	if err := metric.WithLabelValues(string(RequestPanicPlugin), string(RequestPanicPreCommit)).
		Write(value); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := value.GetCounter().GetValue(); got != 1 {
		t.Fatalf("pre-commit count = %v, want 1", got)
	}
}
