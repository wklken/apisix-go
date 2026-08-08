package otel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
)

func TestInitTracerProviderLifecycleAndSampling(t *testing.T) {
	provider, err := InitTracerProvider("unit-service")
	if err != nil {
		t.Fatalf("InitTracerProvider() error = %v", err)
	}
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

func TestOtelShutdownOwnedByCaller(t *testing.T) {
	shutdown, err := Init("unit-service")
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init() shutdown = nil, want an idempotent stop function")
	}

	// The tracer provider must still be usable after Init returns: the old
	// init() deferred Shutdown and killed the batcher immediately.
	tracer := otel.GetTracerProvider().Tracer("unit-test")
	_, span := tracer.Start(context.Background(), "after-init")
	span.End()

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}
