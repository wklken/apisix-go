package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type executorTraceKey struct{}

type executorTrace struct {
	mu      sync.Mutex
	markers []string
}

func (t *executorTrace) add(marker string) {
	t.mu.Lock()
	t.markers = append(t.markers, marker)
	t.mu.Unlock()
}

func (t *executorTrace) values() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.markers...)
}

type executorLegacyPlugin struct {
	base.BasePlugin
	handler func(http.Handler) http.Handler
}

func (p *executorLegacyPlugin) Init() error     { return nil }
func (p *executorLegacyPlugin) PostInit() error { return nil }
func (p *executorLegacyPlugin) Config() any     { return nil }
func (p *executorLegacyPlugin) Handler(next http.Handler) http.Handler {
	if p.handler != nil {
		return p.handler(next)
	}
	return next
}

type executorRequestPlugin struct {
	executorLegacyPlugin
	phase func(http.ResponseWriter, *http.Request) base.RequestPhaseResult
}

func (p *executorRequestPlugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	return p.phase(w, r)
}

func newExecutorLegacyPlugin(name string, priority int, handler func(http.Handler) http.Handler) *executorLegacyPlugin {
	plugin := &executorLegacyPlugin{handler: handler}
	plugin.Name = name
	plugin.SetPriority(priority)
	return plugin
}

func newExecutorRequestPlugin(
	name string,
	priority int,
	phase func(http.ResponseWriter, *http.Request) base.RequestPhaseResult,
) *executorRequestPlugin {
	plugin := &executorRequestPlugin{phase: phase}
	plugin.Name = name
	plugin.SetPriority(priority)
	return plugin
}

func executorRequest(t *testing.T) (*http.Request, *apisixctx.RequestLifecycle, *executorTrace) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	trace := &executorTrace{}
	request = request.WithContext(context.WithValue(request.Context(), executorTraceKey{}, trace))
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	return request, lifecycle, trace
}

func executorTraceFromRequest(r *http.Request) *executorTrace {
	trace, _ := r.Context().Value(executorTraceKey{}).(*executorTrace)
	return trace
}

func TestExecutorMixedRequestAndLegacyPreservesPriorityAndUnwind(t *testing.T) {
	high := newExecutorRequestPlugin(
		"explicit-high",
		300,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("explicit-high")
			return base.ContinueRequest(r)
		},
	)
	legacy := newExecutorLegacyPlugin("legacy", 200, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("legacy-enter")
			next.ServeHTTP(w, r)
			executorTraceFromRequest(r).add("legacy-exit")
		})
	})
	low := newExecutorRequestPlugin(
		"explicit-low",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("explicit-low")
			return base.ContinueRequest(r)
		},
	)

	request, lifecycle, trace := executorRequest(t)
	terminal := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})
	NewExecutor(high, legacy, low).Then(terminal).ServeHTTP(httptest.NewRecorder(), request)

	want := []string{"explicit-high", "legacy-enter", "explicit-low", "terminal", "legacy-exit"}
	if got := trace.values(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed executor order = %v, want %v", got, want)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceUpstream {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceUpstream)
	}
}

func TestExecutorStopsWithoutCallingRemainder(t *testing.T) {
	stop := newExecutorRequestPlugin("stop", 300, func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		executorTraceFromRequest(r).add("stop")
		return base.StopRequest(r)
	})
	legacy := newExecutorLegacyPlugin("legacy", 200, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("legacy")
			next.ServeHTTP(w, r)
		})
	})
	request, lifecycle, trace := executorRequest(t)
	NewExecutor(stop, legacy).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)

	if got, want := trace.values(), []string{"stop"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped executor order = %v, want %v", got, want)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestExecutorPropagatesReplacementRequest(t *testing.T) {
	request, lifecycle, trace := executorRequest(t)
	replacement := request.WithContext(request.Context())
	replacement.Header.Set("X-Replacement", "yes")
	explicit := newExecutorRequestPlugin(
		"replacement",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			return base.ContinueRequest(replacement)
		},
	)
	legacy := newExecutorLegacyPlugin("legacy", 50, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r != replacement || r.Header.Get("X-Replacement") != "yes" {
				t.Errorf(
					"legacy request = %p/%q, want replacement %p/yes",
					r,
					r.Header.Get("X-Replacement"),
					replacement,
				)
			}
			executorTraceFromRequest(r).add("legacy")
			next.ServeHTTP(w, r)
		})
	})
	var terminalRequest *http.Request
	terminal := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { terminalRequest = r })

	NewExecutor(explicit, legacy).Then(terminal).ServeHTTP(httptest.NewRecorder(), request)
	if terminalRequest != replacement {
		t.Fatalf("terminal request = %p, want replacement %p", terminalRequest, replacement)
	}
	if lifecycle.FinalRequest() != replacement {
		t.Fatalf("FinalRequest() = %p, want replacement %p", lifecycle.FinalRequest(), replacement)
	}
	if got := trace.values(); !reflect.DeepEqual(got, []string{"legacy"}) {
		t.Fatalf("trace = %v, want [legacy]", got)
	}
}

func TestExecutorTreatsUnknownDecisionAsStop(t *testing.T) {
	unknown := newExecutorRequestPlugin(
		"unknown",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			return base.RequestPhaseResult{Request: r, Decision: base.RequestDecision(42)}
		},
	)
	request, lifecycle, _ := executorRequest(t)
	called := false
	NewExecutor(unknown).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if called {
		t.Fatal("terminal called for unknown decision")
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestExecutorDoesNotMutateCallerSlice(t *testing.T) {
	low := newExecutorLegacyPlugin("low", 10, nil)
	high := newExecutorLegacyPlugin("high", 100, nil)
	plugins := []Plugin{low, high}
	_ = NewExecutor(plugins...)
	if plugins[0] != low || plugins[1] != high {
		t.Fatalf(
			"NewExecutor mutated caller slice: got [%s %s], want [low high]",
			plugins[0].GetName(),
			plugins[1].GetName(),
		)
	}
}

func TestExecutorPreservesTransformPipeline(t *testing.T) {
	transform := func(name, marker string, priority int) *executorLegacyPlugin {
		return newExecutorLegacyPlugin(name, priority, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buffered := base.GetOrCreateTransformResponseWriter(r)
				_, _ = buffered.Write([]byte(marker))
				next.ServeHTTP(buffered, r)
				buffered.Commit(w)
			})
		})
	}
	outer := transform("echo", "outer", 200)
	inner := transform("response-rewrite", "inner", 100)
	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("terminal")) })
	request, _, _ := executorRequest(t)
	response := httptest.NewRecorder()
	NewExecutor(outer, inner).Then(terminal).ServeHTTP(response, request)
	if got := response.Body.String(); got != "outerinnerterminal" {
		t.Fatalf("transform pipeline body = %q, want outerinnerterminal", got)
	}
}

func TestExecutorTerminalSourcePrecedence(t *testing.T) {
	for _, source := range []apisixctx.ResponseSource{
		apisixctx.ResponseSourceEarlyStop,
		apisixctx.ResponseSourceCacheHit,
	} {
		t.Run(string(source), func(t *testing.T) {
			request, lifecycle, _ := executorRequest(t)
			lifecycle.SetResponseSource(source)
			var terminalRequest *http.Request
			NewExecutor().Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				terminalRequest = r
			})).ServeHTTP(httptest.NewRecorder(), request)
			if terminalRequest != request {
				t.Fatalf("terminal request = %p, want %p", terminalRequest, request)
			}
			if got := lifecycle.ResponseSource(); got != source {
				t.Fatalf("ResponseSource() = %q, want %q", got, source)
			}
			if got := lifecycle.FinalRequest(); got != request {
				t.Fatalf("FinalRequest() = %p, want %p", got, request)
			}
		})
	}
}
