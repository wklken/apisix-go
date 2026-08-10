package plugin

import (
	"context"
	"fmt"
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

func (p *executorRequestPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	return p.phase(w, r)
}

func newExecutorLegacyPlugin(
	name string,
	priority int,
	handler func(http.Handler) http.Handler,
) *executorLegacyPlugin {
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
	stop := newExecutorRequestPlugin(
		"stop",
		300,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("stop")
			return base.StopRequest(r)
		},
	)
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
	terminal := http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) { terminalRequest = r },
	)

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
	NewExecutor(
		unknown,
	).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).
		ServeHTTP(
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
	terminal := http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("terminal")) },
	)
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

func TestScopedExecutorClonesBindings(t *testing.T) {
	low := newExecutorLegacyPlugin("low", 10, nil)
	high := newExecutorLegacyPlugin("high", 100, nil)
	bindings := []Binding{
		BindPlugin(
			"legacy-low",
			low,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "low"},
		),
		BindPlugin(
			"legacy-high",
			high,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "high"},
		),
	}
	executor := NewScopedExecutor(bindings...)
	bindings[0] = Binding{}
	if len(executor.bindings) != 2 {
		t.Fatalf("scoped executor bindings = %d, want 2", len(executor.bindings))
	}
	if executor.bindings[0].Plugin != low || executor.bindings[1].Plugin != high {
		t.Fatalf("scoped executor did not retain cloned input bindings: %#v", executor.bindings)
	}
}

func TestScopedExecutorPreservesResourceProvenance(t *testing.T) {
	plugin := newExecutorLegacyPlugin("rewrite-impl", 10, nil)
	want := ResourceProvenance{Kind: ResourcePluginConfig, ID: "pc-7"}
	executor := NewScopedExecutor(BindPlugin("real-ip", plugin, ScopeRoute, want))
	if got := executor.bindings[0].Provenance; got != want {
		t.Fatalf("binding provenance = %#v, want %#v", got, want)
	}
}

func TestScopedExecutorRunsGlobalRewriteBeforeHigherPriorityRouteRewrite(t *testing.T) {
	global := newExecutorRequestPlugin(
		"global-impl",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("global")
			return base.ContinueRequest(r)
		},
	)
	route := newExecutorRequestPlugin(
		"route-impl",
		10000,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("route")
			return base.ContinueRequest(r)
		},
	)
	request, _, trace := executorRequest(t)
	NewScopedExecutor(
		BindPlugin(
			"request-id",
			global,
			ScopeGlobal,
			ResourceProvenance{Kind: ResourceGlobalRule, ID: "g"},
		),
		BindPlugin(
			"request-id",
			route,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "r"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)

	if got, want := trace.values(), []string{"global", "route", "terminal"}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("scoped executor order = %v, want %v", got, want)
	}
}

func TestScopedExecutorSortsRewritePriorityWithinScope(t *testing.T) {
	low := newExecutorRequestPlugin(
		"global-low",
		10,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("low")
			return base.ContinueRequest(r)
		},
	)
	high := newExecutorRequestPlugin(
		"global-high",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("high")
			return base.ContinueRequest(r)
		},
	)
	request, _, trace := executorRequest(t)
	NewScopedExecutor(
		BindPlugin(
			"request-id",
			low,
			ScopeGlobal,
			ResourceProvenance{Kind: ResourceGlobalRule, ID: "low"},
		),
		BindPlugin(
			"request-id",
			high,
			ScopeGlobal,
			ResourceProvenance{Kind: ResourceGlobalRule, ID: "high"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)
	if got, want := trace.values(), []string{"high", "low", "terminal"}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("same-scope rewrite order = %v, want %v", got, want)
	}
}

func TestScopedExecutorStopsRewriteBeforeLegacyRemainder(t *testing.T) {
	stop := newExecutorRequestPlugin(
		"stop",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("stop")
			return base.StopRequest(r)
		},
	)
	legacy := newExecutorLegacyPlugin("legacy", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("legacy")
			next.ServeHTTP(w, r)
		})
	})
	request, lifecycle, trace := executorRequest(t)
	NewScopedExecutor(
		BindPlugin(
			"request-id",
			stop,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "stop"},
		),
		BindPlugin(
			"legacy",
			legacy,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "legacy"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)

	if got, want := trace.values(), []string{"stop"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped scoped executor order = %v, want %v", got, want)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestScopedExecutorPropagatesRequestAcrossScopes(t *testing.T) {
	request, _, trace := executorRequest(t)
	replacement := request.WithContext(
		context.WithValue(request.Context(), executorTraceKey{}, trace),
	)
	global := newExecutorRequestPlugin(
		"global",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			return base.ContinueRequest(replacement)
		},
	)
	route := newExecutorRequestPlugin(
		"route",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			if r != replacement {
				t.Errorf("route request = %p, want replacement %p", r, replacement)
			}
			executorTraceFromRequest(r).add("route")
			return base.ContinueRequest(r)
		},
	)
	var terminalRequest *http.Request
	NewScopedExecutor(
		BindPlugin(
			"request-id",
			global,
			ScopeGlobal,
			ResourceProvenance{Kind: ResourceGlobalRule, ID: "g"},
		),
		BindPlugin(
			"request-id",
			route,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "r"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { terminalRequest = r })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if terminalRequest != replacement {
		t.Fatalf("terminal request = %p, want replacement %p", terminalRequest, replacement)
	}
	if got, want := trace.values(), []string{"route"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("trace = %v, want %v", got, want)
	}
}

func TestScopedExecutorLeavesLegacyPriorityAndUnwindUnchanged(t *testing.T) {
	high := newExecutorLegacyPlugin("high", 300, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("high-enter")
			next.ServeHTTP(w, r)
			executorTraceFromRequest(r).add("high-exit")
		})
	})
	low := newExecutorLegacyPlugin("low", 100, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("low-enter")
			next.ServeHTTP(w, r)
			executorTraceFromRequest(r).add("low-exit")
		})
	})
	request, _, trace := executorRequest(t)
	NewScopedExecutor(
		BindPlugin(
			"legacy-high",
			high,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "high"},
		),
		BindPlugin(
			"legacy-low",
			low,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "low"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)

	want := []string{"high-enter", "low-enter", "terminal", "low-exit", "high-exit"}
	if got := trace.values(); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy remainder order = %v, want %v", got, want)
	}
}

type executorAuthStateKey struct{}

func TestScopedExecutorKeepsRouteRewriteInLegacyAuthEnvelope(t *testing.T) {
	global := newExecutorRequestPlugin(
		"global-rewrite",
		-100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			executorTraceFromRequest(r).add("global-rewrite")
			return base.ContinueRequest(r)
		},
	)
	auth := newExecutorLegacyPlugin("jwt-auth", 100, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			executorTraceFromRequest(r).add("auth-enter")
			r = r.WithContext(
				context.WithValue(r.Context(), executorAuthStateKey{}, "authenticated"),
			)
			next.ServeHTTP(w, r)
			executorTraceFromRequest(r).add("auth-exit")
		})
	})
	routeRewrite := newExecutorRequestPlugin(
		"request-id",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			if got := r.Context().Value(executorAuthStateKey{}); got != "authenticated" {
				executorTraceFromRequest(r).add("route-rewrite-before-auth")
				return base.StopRequest(r)
			}
			executorTraceFromRequest(r).add("route-rewrite")
			return base.ContinueRequest(r)
		},
	)

	request, lifecycle, trace := executorRequest(t)
	NewScopedExecutor(
		BindPlugin(
			"request-id",
			global,
			ScopeGlobal,
			ResourceProvenance{Kind: ResourceGlobalRule, ID: "global"},
		),
		BindPlugin(
			"jwt-auth",
			auth,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-auth"},
		),
		BindPlugin(
			"request-id",
			routeRewrite,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-rewrite"},
		),
	).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		executorTraceFromRequest(r).add("terminal")
	})).ServeHTTP(httptest.NewRecorder(), request)

	want := []string{"global-rewrite", "auth-enter", "route-rewrite", "terminal", "auth-exit"}
	if got := trace.values(); !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped rewrite/auth order = %v, want %v", got, want)
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceUpstream {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceUpstream)
	}
}

func pipelineBinding(name string, p Plugin, scope Scope, priority int) Binding {
	if setter, ok := p.(interface{ SetPriority(int) }); ok {
		setter.SetPriority(priority)
	}
	return BindPlugin(name, p, scope, ResourceProvenance{Kind: ResourceRoute, ID: name})
}

func TestRequestPipelineRunsPlan14V2Order(t *testing.T) {
	order := []string{}
	mark := func(name string) func(http.ResponseWriter, *http.Request) base.RequestPhaseResult {
		return func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, name)
			return base.ContinueRequest(r)
		}
	}
	bindings := []Binding{
		pipelineBinding(
			"request-context",
			newExecutorRequestPlugin("system", 1, mark("system")),
			ScopeSystem,
			1,
		),
		pipelineBinding(
			"request-id",
			newExecutorRequestPlugin("global", 1, mark("global")),
			ScopeGlobal,
			1,
		),
		pipelineBinding(
			"jwt-auth",
			newExecutorRequestPlugin("auth", 1, mark("auth")),
			ScopeRoute,
			1,
		),
		pipelineBinding(
			"proxy-rewrite",
			newExecutorRequestPlugin("route-rewrite", 1, mark("route-rewrite")),
			ScopeRoute,
			1,
		),
		pipelineBinding(
			"attach-consumer-label",
			newExecutorRequestPlugin("global-consumer-rewrite", 1, mark("global-consumer-rewrite")),
			ScopeGlobal,
			1,
		),
		pipelineBinding(
			"acl",
			newExecutorRequestPlugin("global-access", 1, mark("global-access")),
			ScopeGlobal,
			1,
		),
		pipelineBinding(
			"limit-conn",
			newExecutorRequestPlugin("route-access", 1, mark("route-access")),
			ScopeRoute,
			1,
		),
	}
	dynamic := pipelineBinding(
		"attach-consumer-label",
		newExecutorRequestPlugin("consumer-rewrite", 1, mark("consumer-rewrite")),
		ScopeConsumer,
		1,
	)
	resolverCalls := 0
	pipeline := NewRequestPipeline(bindings, func(r *http.Request) (ConsumerResolution, error) {
		resolverCalls++
		return ConsumerResolution{Request: r, Bindings: []Binding{dynamic}, Resolved: true}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = apisixctx.WithBeforeProxyHook(
		request,
		func(*http.Request) { order = append(order, "before-proxy") },
	)
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "terminal")
	})).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)

	want := []string{
		"system", "global", "auth", "route-rewrite", "global-consumer-rewrite",
		"consumer-rewrite", "global-access", "route-access", "before-proxy", "terminal",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("pipeline order = %v, want %v", order, want)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
}

func TestRequestPipelineRunsSystemAccessBeforeGlobalAndMergedAccess(t *testing.T) {
	order := []string{}
	mark := func(name string) func(http.ResponseWriter, *http.Request) base.RequestPhaseResult {
		return func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, name)
			return base.ContinueRequest(r)
		}
	}
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding(
			"client-control",
			newExecutorRequestPlugin("system-access", 1, mark("system-access")),
			ScopeSystem,
			1,
		),
		pipelineBinding("acl", newExecutorRequestPlugin("global-access", 1, mark("global-access")), ScopeGlobal, 1),
		pipelineBinding("limit-conn", newExecutorRequestPlugin("route-access", 1, mark("route-access")), ScopeRoute, 1),
	}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "terminal")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"system-access", "global-access", "route-access", "terminal"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("access order = %v, want %v", order, want)
	}
}

func TestRequestPipelineAuthStopSkipsResolverAndLegacy(t *testing.T) {
	order := []string{}
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, "auth")
			return base.StopRequest(r)
		},
	)
	legacy := newExecutorLegacyPlugin("legacy", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "legacy")
			next.ServeHTTP(w, r)
		})
	})
	resolverCalls := 0
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
		pipelineBinding("unknown", legacy, ScopeRoute, 1),
	}, func(*http.Request) (ConsumerResolution, error) {
		resolverCalls++
		return ConsumerResolution{Resolved: true}, nil
	})
	terminalCalls := 0
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalls++ })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if !reflect.DeepEqual(order, []string{"auth"}) {
		t.Fatalf("auth-stop order = %v, want [auth]", order)
	}
	if resolverCalls != 0 || terminalCalls != 0 {
		t.Fatalf("resolver/terminal calls = %d/%d, want 0/0", resolverCalls, terminalCalls)
	}
}

func TestRequestPipelineResolverErrorSkipsLegacyAndReturnsGeneric500(t *testing.T) {
	legacyCalls := 0
	legacy := newExecutorLegacyPlugin("legacy", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			legacyCalls++
			next.ServeHTTP(w, r)
		})
	})
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("unknown", legacy, ScopeRoute, 1),
	}, func(*http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{}, fmt.Errorf("missing group: store unavailable")
	})
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") })).
		ServeHTTP(
			response,
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() != "Internal Server Error\n" {
		t.Fatalf(
			"resolver error response = %d/%q, want generic 500",
			response.Code,
			response.Body.String(),
		)
	}
	if legacyCalls != 0 {
		t.Fatalf("legacy calls = %d, want 0", legacyCalls)
	}
}

func TestRequestPipelineResolvesExactlyOnceWithoutAuthentication(t *testing.T) {
	resolverCalls := 0
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		resolverCalls++
		return ConsumerResolution{Request: r, Resolved: false}, nil
	})
	terminalCalls := 0
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { terminalCalls++ })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if resolverCalls != 1 || terminalCalls != 1 {
		t.Fatalf("resolver/terminal calls = %d/%d, want 1/1", resolverCalls, terminalCalls)
	}
}

func TestRequestPipelineConsumerOverridesRouteRewriteBeforeExecution(t *testing.T) {
	order := []string{}
	route := newExecutorRequestPlugin(
		"route",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, "route")
			return base.ContinueRequest(r)
		},
	)
	consumer := newExecutorRequestPlugin(
		"consumer",
		1,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, "consumer")
			return base.ContinueRequest(r)
		},
	)
	pipeline := NewRequestPipeline(
		[]Binding{pipelineBinding("proxy-rewrite", route, ScopeRoute, 1)},
		func(r *http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{
				Request:  r,
				Resolved: true,
				Bindings: []Binding{pipelineBinding("proxy-rewrite", consumer, ScopeConsumer, 1)},
			}, nil
		},
	)
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "terminal")
	})).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
	)
	if got, want := order, []string{"consumer", "terminal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("consumer override order = %v, want %v", got, want)
	}
}

func TestRequestPipelineMergedAccessUsesEffectivePriorityAcrossScopes(t *testing.T) {
	order := []string{}
	routeAccess := newExecutorRequestPlugin(
		"route-access",
		200,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, "route-access")
			return base.ContinueRequest(r)
		},
	)
	consumerAccess := newExecutorRequestPlugin(
		"consumer-access",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, "consumer-access")
			return base.ContinueRequest(r)
		},
	)
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("limit-conn", routeAccess, ScopeRoute, 200),
	}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{
			Request:  r,
			Resolved: true,
			Bindings: []Binding{pipelineBinding("acl", consumerAccess, ScopeConsumer, 100)},
		}, nil
	})
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		order = append(order, "terminal")
	})).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/", nil),
	)
	want := []string{"route-access", "consumer-access", "terminal"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("merged access order = %v, want %v", order, want)
	}
}

func TestRequestPipelineDeferredLegacyUsesEffectiveWinnersOnly(t *testing.T) {
	order := []string{}
	legacy := func(name string) *executorLegacyPlugin {
		return newExecutorLegacyPlugin(name, 1, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		})
	}
	routeLoser := legacy("route-loser")
	consumerWinner := legacy("consumer-winner")
	pipeline := NewRequestPipeline(
		[]Binding{pipelineBinding("same-name", routeLoser, ScopeRoute, 1)},
		func(r *http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{
				Request:  r,
				Resolved: true,
				Bindings: []Binding{pipelineBinding("same-name", consumerWinner, ScopeConsumer, 1)},
			}, nil
		},
	)
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { order = append(order, "terminal") })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	if got, want := order, []string{"consumer-winner", "terminal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("effective legacy order = %v, want %v", got, want)
	}
}

func TestRequestPipelineDeferredLegacyPreservesPriorityAndUnwind(t *testing.T) {
	order := []string{}
	legacy := func(name string, priority int) *executorLegacyPlugin {
		return newExecutorLegacyPlugin(name, priority, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-enter")
				next.ServeHTTP(w, r)
				order = append(order, name+"-exit")
			})
		})
	}
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("global-legacy", legacy("global", 300), ScopeGlobal, 300),
		pipelineBinding("route-legacy", legacy("route", 100), ScopeRoute, 100),
	}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { order = append(order, "terminal") })).
		ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	want := []string{"global-enter", "route-enter", "terminal", "route-exit", "global-exit"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("legacy priority/unwind = %v, want %v", order, want)
	}
}

func TestRequestPipelinePropagatesEveryReplacementRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	first := request.WithContext(context.WithValue(request.Context(), executorTraceKey{}, "first"))
	second := first.WithContext(context.WithValue(first.Context(), executorTraceKey{}, "second"))
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(_ http.ResponseWriter, _ *http.Request) base.RequestPhaseResult {
			return base.ContinueRequest(first)
		},
	)
	rewrite := newExecutorRequestPlugin(
		"rewrite",
		1,
		func(_ http.ResponseWriter, _ *http.Request) base.RequestPhaseResult {
			return base.ContinueRequest(second)
		},
	)
	var terminalRequest *http.Request
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
		pipelineBinding("proxy-rewrite", rewrite, ScopeRoute, 1),
	}, func(r *http.Request) (ConsumerResolution, error) {
		if r != first {
			t.Fatalf("resolver request = %p, want auth replacement %p", r, first)
		}
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	pipeline.Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { terminalRequest = r })).
		ServeHTTP(
			httptest.NewRecorder(),
			request,
		)
	if terminalRequest != second {
		t.Fatalf("terminal request = %p, want rewrite replacement %p", terminalRequest, second)
	}
}

func TestRequestPipelineRunsBeforeProxyOnce(t *testing.T) {
	hookCalls := 0
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = apisixctx.WithBeforeProxyHook(request, func(*http.Request) { hookCalls++ })
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if hookCalls != 1 {
		t.Fatalf("before-proxy hook calls = %d, want 1", hookCalls)
	}
}
