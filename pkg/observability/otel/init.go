package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	stdout "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Init installs the global tracer provider and text propagators and returns
// an idempotent shutdown function owned by the caller. The old init() used a
// deferred Shutdown that ran while init returned, leaving a dead batcher as
// the global provider; shutdown must be driven by the process owner instead.
func Init(serviceName string) (func(context.Context) error, error) {
	provider, err := InitTracerProvider(serviceName)
	if err != nil {
		return nil, err
	}
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	)
	return provider.Shutdown, nil
}

func InitTracerProvider(serviceName string) (*sdktrace.TracerProvider, error) {
	exporter, err := stdout.New(stdout.WithPrettyPrint())
	if err != nil {
		return nil, fmt.Errorf("create trace exporter: %w", err)
	}
	res, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			// the service name used to display traces in backends
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize resource: %w", err)
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	), nil
}
