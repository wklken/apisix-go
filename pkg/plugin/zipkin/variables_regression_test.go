package zipkin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestZipkinSetNgxVar(t *testing.T) {
	p := &Plugin{config: Config{Endpoint: "http://127.0.0.1:9411/api/v2/spans", SampleRatio: 1}}
	p.SetDependencies(base.Dependencies{
		Tasks: newLoggerTestTaskOwner(t),
		Config: &config.EffectiveConfig{
			Config: config.Config{PluginAttr: map[string]map[string]any{"zipkin": {"set_ngx_var": true}}},
		},
	})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/echo", nil), time.Now(),
	)
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("decision = %d, want continue", result.Decision)
	}
	lifecycle.Complete(
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Now(),
	)
	_ = lifecycle.Finalize()
	if result.Request == nil {
		t.Fatal("RunRequestPhase returned nil request")
	}
	traceID, _ := apisixctx.GetRequestVar(result.Request, "$zipkin_trace_id").(string)
	spanID, _ := apisixctx.GetRequestVar(result.Request, "$zipkin_span_id").(string)
	parent, _ := apisixctx.GetRequestVar(result.Request, "$zipkin_context_traceparent").(string)
	if traceID == "" || spanID == "" || parent == "" {
		t.Fatalf(
			"zipkin ngx vars empty: trace_id=%q span_id=%q traceparent=%q, want APISIX 3.17 set_ngx_var values",
			traceID,
			spanID,
			parent,
		)
	}
}
