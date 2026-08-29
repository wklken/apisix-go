package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// benchmarkRequestPhase keeps the benchmark focused on pipeline materialization
// rather than a plugin's request-specific work. It implements the same
// request-phase contract used by the registered request-stage plugins.
type benchmarkRequestPhase struct {
	base.BasePlugin
}

func newBenchmarkRequestPhase(name string) *benchmarkRequestPhase {
	plugin := &benchmarkRequestPhase{}
	plugin.Name = name
	plugin.Priority = 100
	return plugin
}

func (p *benchmarkRequestPhase) Init() error     { return nil }
func (p *benchmarkRequestPhase) PostInit() error { return nil }
func (p *benchmarkRequestPhase) Config() any     { return nil }
func (p *benchmarkRequestPhase) Handler(next http.Handler) http.Handler {
	return base.AdaptRequestPhase(p, next)
}

func (p *benchmarkRequestPhase) RunRequestPhase(
	_ http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	return base.ContinueRequest(r)
}

type benchmarkLogPhase struct {
	base.BasePlugin
}

func newBenchmarkLogPhase(name string) *benchmarkLogPhase {
	plugin := &benchmarkLogPhase{}
	plugin.Name = name
	plugin.Priority = 1
	return plugin
}

func (p *benchmarkLogPhase) Init() error     { return nil }
func (p *benchmarkLogPhase) PostInit() error { return nil }
func (p *benchmarkLogPhase) Config() any     { return nil }
func (p *benchmarkLogPhase) Handler(next http.Handler) http.Handler {
	return next
}
func (p *benchmarkLogPhase) RunLogPhase(base.LogSnapshot) error { return nil }

// BenchmarkRequestPipelineHotPath measures the production-shaped request
// pipeline with one static binding. The resolved row adds a distinct consumer
// binding so it exercises the real override/materialization path rather than
// replacing the static request-id binding with the same factory identity.
func BenchmarkRequestPipelineHotPath(b *testing.B) {
	static := bindPluginForTest(
		"request-id",
		newBenchmarkRequestPhase("benchmark-static-request-id"),
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "benchmark"},
	)
	dynamic := bindPluginForTest(
		"proxy-rewrite",
		newBenchmarkRequestPhase("benchmark-consumer-proxy-rewrite"),
		ScopeConsumer,
		ResourceProvenance{Kind: ResourceConsumer, ID: "benchmark-consumer"},
	)
	loggerBinding := bindPluginForTest(
		"http-logger",
		newBenchmarkLogPhase("benchmark-http-logger"),
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "benchmark"},
	)
	staticBindings := []Binding{static, loggerBinding}
	logExecutor, err := NewLogExecutorFromBindings(staticBindings)
	if err != nil {
		b.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	streaming, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		b.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	for _, resolved := range []bool{false, true} {
		name := "static-unresolved"
		if resolved {
			name = "consumer-resolved"
		}
		b.Run(name, func(b *testing.B) {
			resolver := func(r *http.Request) (ConsumerResolution, error) {
				result := ConsumerResolution{Request: r, Resolved: resolved}
				if resolved {
					result.Bindings = []Binding{dynamic}
				}
				return result, nil
			}
			handler := NewRequestPipeline(staticBindings, resolver).
				WithLogExecutor(&logExecutor).
				WithStreamingResponseExecutor(streaming).
				Then(terminal)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				request, lifecycle := apisixctx.EnsureRequestLifecycle(
					httptest.NewRequest(http.MethodGet, "http://gateway.test/bench", nil),
					time.Unix(1, 0),
				)
				writer, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
				request = base.WithResponseCapture(request, capture)
				handler.ServeHTTP(writer, request)
				lifecycle.Complete(capture.Outcome(), time.Unix(2, 0))
				if failures := lifecycle.Finalize(); len(failures) != 0 {
					b.Fatalf("Finalize() failures = %#v", failures)
				}
			}
		})
	}
}
