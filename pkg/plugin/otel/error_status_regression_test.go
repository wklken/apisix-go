package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// `{response_source} error: {status}` for HTTP >= 500.
func TestOTelErrorStatusDescription(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	p := &Plugin{tracerProvider: provider}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Request != nil {
		request = result.Request
	}
	apisixctx.SetResponseSource(request, apisixctx.ResponseSourceUpstream)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{
			Kind:      apisixctx.RequestOutcomeCompleted,
			Status:    http.StatusBadGateway,
			Committed: true,
		},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("finalize failures = %#v", failures)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	got := spans[0].Status().Description
	want := "upstream error: 502"
	if got != want {
		t.Fatalf("status description = %q, want APISIX 3.17 %q", got, want)
	}
}
