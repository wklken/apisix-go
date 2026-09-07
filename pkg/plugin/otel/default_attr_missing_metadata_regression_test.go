package otel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestMissingMetadataStillNoOpsWithDefaultPluginAttr(t *testing.T) {
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	defer otel.SetTextMapPropagator(previous)
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{
		Config: &config.EffectiveConfig{Config: config.Config{PluginAttr: map[string]map[string]any{
			"opentelemetry": {
				"trace_id_source": "x-request-id",
				"collector":       map[string]any{"address": "127.0.0.1:4318", "request_timeout": 3},
			},
		}}},
		Metadata: mustOpenTelemetryMetadataView(t, map[string]string{}),
		Tasks:    newOpenTelemetryTaskOwner(t),
	})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	*p.Config().(*Config) = Config{Sampler: SamplerConfig{Name: "always_on"}}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	req, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders", nil),
		time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), req)
	if got := result.Request.Header.Get("traceparent"); got != "" {
		t.Fatalf("traceparent = %q; APISIX 3.17 returns before tracer creation when plugin_metadata is absent", got)
	}
}
