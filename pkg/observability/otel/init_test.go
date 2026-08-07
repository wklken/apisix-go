package otel

import (
	"context"
	"testing"
)

func TestInitTracerProviderLifecycleAndSampling(t *testing.T) {
	provider := InitTracerProvider("unit-service")
	tracer := provider.Tracer("unit-test")

	_, span := tracer.Start(context.Background(), "operation")
	if !span.SpanContext().IsSampled() {
		t.Fatal("span is not sampled, want AlwaysSample provider")
	}
	span.End()

	if err := provider.ForceFlush(t.Context()); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	if err := provider.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}
