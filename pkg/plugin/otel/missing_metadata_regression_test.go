package otel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// missing (no extract/inject). Go still injects traceparent.
func TestOTelRequiresPluginMetadata(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })

	p := &Plugin{}
	p.SetDependencies(base.Dependencies{
		Config:   &config.EffectiveConfig{},
		Metadata: mustOpenTelemetryMetadataView(t, map[string]string{}),
		Tasks:    newOpenTelemetryTaskOwner(t),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Now(),
	)
	_ = lifecycle.Finalize()
	if result.Request == nil {
		t.Fatal("nil request")
	}
	if got := result.Request.Header.Get("traceparent"); got != "" {
		t.Fatalf("traceparent = %q, want empty when plugin_metadata is absent (APISIX 3.17 no-op)", got)
	}
}
