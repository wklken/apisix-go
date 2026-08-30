package plugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	corsplugin "github.com/wklken/apisix-go/pkg/plugin/cors"
)

type executorTraceKey struct{}

func executorRequest(t *testing.T) (*http.Request, *apisixctx.RequestLifecycle) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
	return request, lifecycle
}

type executorLegacyPlugin struct {
	base.BasePlugin
	handler func(http.Handler) http.Handler
}

type constructionCountingPlugin struct {
	base.BasePlugin
	constructed atomic.Int64
	served      atomic.Int64
}

func (p *constructionCountingPlugin) Init() error     { return nil }
func (p *constructionCountingPlugin) PostInit() error { return nil }
func (p *constructionCountingPlugin) Config() any     { return nil }

func (p *constructionCountingPlugin) Handler(next http.Handler) http.Handler {
	p.constructed.Add(1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.served.Add(1)
		next.ServeHTTP(w, r)
	})
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

func pipelineBinding(name string, p Plugin, scope Scope, priority int) Binding {
	if setter, ok := p.(interface{ SetPriority(int) }); ok {
		setter.SetPriority(priority)
	}
	binding := bindPluginForTest(name, p, scope, ResourceProvenance{Kind: ResourceRoute, ID: name})
	binding.Priority = priority
	return binding
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
			"example-plugin",
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
		func(*http.Request) error {
			order = append(order, "before-proxy")
			return nil
		},
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
		response.Body.String() != `{"message":"Internal Server Error"}` {
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

func TestRequestPipelinePrebuildsStaticHandler(t *testing.T) {
	plugin := &constructionCountingPlugin{}
	plugin.Name = "construction-counting"
	binding := bindPluginForTest(
		"construction-counting",
		plugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "static"},
	)
	terminalCalls := 0
	pipeline := NewRequestPipeline([]Binding{binding}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: false}, nil
	})
	handler := pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		terminalCalls++
	}))
	for range 2 {
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}
	if got := plugin.constructed.Load(); got != 1 {
		t.Fatalf("handler constructions = %d, want 1", got)
	}
	if got := plugin.served.Load(); got != 2 {
		t.Fatalf("handler requests = %d, want 2", got)
	}
	if terminalCalls != 2 {
		t.Fatalf("terminal calls = %d, want 2", terminalCalls)
	}
}

func TestRequestPipelineStaticHookPerRequest(t *testing.T) {
	var hookCalls int
	var replacements []*http.Request
	var terminalRequests []*http.Request
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: false}, nil
	})
	handler := pipeline.ThenWithPostResolutionHook(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			terminalRequests = append(terminalRequests, r)
		}),
		func(r *http.Request, _ EffectiveBindingSet) (*http.Request, error) {
			hookCalls++
			replacement := r.WithContext(context.WithValue(r.Context(), executorTraceKey{}, hookCalls))
			replacements = append(replacements, replacement)
			return replacement, nil
		},
	)
	for range 2 {
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}
	if hookCalls != 2 {
		t.Fatalf("hook calls = %d, want 2", hookCalls)
	}
	if len(terminalRequests) != len(replacements) {
		t.Fatalf("terminal requests = %d, want hook replacements %d", len(terminalRequests), len(replacements))
	}
	for index := range replacements {
		if terminalRequests[index] != replacements[index] {
			t.Fatalf(
				"terminal request %d = %p, want hook replacement %p",
				index,
				terminalRequests[index],
				replacements[index],
			)
		}
	}
}

func TestRequestPipelineResolvedEmptyUsesDynamicPath(t *testing.T) {
	plugin := &constructionCountingPlugin{}
	plugin.Name = "construction-counting"
	binding := bindPluginForTest(
		"construction-counting",
		plugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "static"},
	)
	var terminalRequests []*http.Request
	pipeline := NewRequestPipeline([]Binding{binding}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	handler := pipeline.ThenWithPostResolutionHook(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			terminalRequests = append(terminalRequests, r)
		}),
		func(r *http.Request, _ EffectiveBindingSet) (*http.Request, error) {
			return r.WithContext(context.WithValue(r.Context(), executorTraceKey{}, "replacement")), nil
		},
	)
	for range 2 {
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}
	if got := plugin.constructed.Load(); got != 3 {
		t.Fatalf("handler constructions = %d, want static build plus two dynamic builds", got)
	}
	if got := plugin.served.Load(); got != 2 {
		t.Fatalf("handler requests = %d, want 2", got)
	}
	if len(terminalRequests) != 2 {
		t.Fatalf("terminal requests = %d, want 2", len(terminalRequests))
	}
	for index, request := range terminalRequests {
		if got := request.Context().Value(executorTraceKey{}); got != "replacement" {
			t.Fatalf("terminal request %d context value = %v, want replacement", index, got)
		}
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
	request = apisixctx.WithBeforeProxyHook(request, func(*http.Request) error {
		hookCalls++
		return nil
	})
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

func TestRequestPipelineAttributesRequestPhasePanics(t *testing.T) {
	tests := []struct {
		name    string
		factory string
		phase   Phase
	}{
		{name: "rewrite", factory: "request-id", phase: PhaseRewrite},
		{name: "access", factory: "limit-conn", phase: PhaseAccess},
		{name: "authentication", factory: "jwt-auth", phase: PhaseAccess},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := &struct{ stage string }{stage: test.name}
			panicking := newExecutorRequestPlugin(test.factory, 1, func(
				http.ResponseWriter,
				*http.Request,
			) base.RequestPhaseResult {
				panic(want)
			})
			handler := NewRequestPipeline([]Binding{
				pipelineBinding(test.factory, panicking, ScopeRoute, 1),
			}, nil).Then(http.NotFoundHandler())

			recovered := recoverHandlerPanic(t, handler)
			panicErr, ok := recovered.(*PanicError)
			if !ok {
				t.Fatalf("panic = %T, want *PanicError", recovered)
			}
			if panicErr.Factory != test.factory || panicErr.Phase != test.phase ||
				panicErr.Value != want || len(panicErr.Stack) == 0 {
				t.Fatalf("panic metadata = %#v", panicErr)
			}
		})
	}
}

func TestLegacyMiddlewareAttributesEntryAndUnwindPanicsAndBuildsOnce(t *testing.T) {
	tests := []struct {
		name    string
		handler func(http.Handler, any) http.Handler
	}{
		{
			name: "entry",
			handler: func(_ http.Handler, anyValue any) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(anyValue) })
			},
		},
		{
			name: "unwind",
			handler: func(next http.Handler, anyValue any) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r)
					panic(anyValue)
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want := &struct{ stage string }{stage: test.name}
			constructions := 0
			legacy := newExecutorLegacyPlugin("legacy", 1, func(next http.Handler) http.Handler {
				constructions++
				return test.handler(next, want)
			})
			handler := NewRequestPipeline([]Binding{
				pipelineBinding("legacy-boundary", legacy, ScopeRoute, 1),
			}, nil).Then(http.NotFoundHandler())
			for range 2 {
				recovered := recoverHandlerPanic(t, handler)
				panicErr, ok := recovered.(*PanicError)
				if !ok {
					t.Fatalf("panic = %T, want *PanicError", recovered)
				}
				if panicErr.Factory != "legacy-boundary" || panicErr.Phase != "" ||
					panicErr.Value != want || len(panicErr.Stack) == 0 {
					t.Fatalf("panic metadata = %#v", panicErr)
				}
			}
			if constructions != 1 {
				t.Fatalf("Handler() constructions = %d, want 1", constructions)
			}
		})
	}
}

func TestRequestPipelineStaticCORSPanicIsAttributedAndHandlerBuildsOnce(t *testing.T) {
	want := &struct{ callback string }{callback: "cors filter"}
	constructions := 0
	cors := newExecutorLegacyPlugin("cors", 4000, func(http.Handler) http.Handler {
		constructions++
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) })
	})
	handler := NewRequestPipeline([]Binding{
		pipelineBinding("cors", cors, ScopeRoute, 4000),
	}, nil).Then(http.NotFoundHandler())
	for range 2 {
		recovered := recoverHandlerPanic(t, handler)
		panicErr, ok := recovered.(*PanicError)
		if !ok {
			t.Fatalf("panic = %T, want *PanicError", recovered)
		}
		if panicErr.Factory != "cors" || panicErr.Phase != PhaseRewrite ||
			panicErr.Value != want || len(panicErr.Stack) == 0 {
			t.Fatalf("panic metadata = %#v", panicErr)
		}
	}
	// CORS has one pre-authentication middleware and one post-resolution
	// rewrite middleware. Both are compiled once and reused by both requests.
	if constructions != 2 {
		t.Fatalf("CORS Handler() constructions = %d, want 2 fixed phase handlers", constructions)
	}
}

func TestRequestPipelineStaticCORSAllowsAuthenticationReplacementWithoutContext(t *testing.T) {
	cors := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
	})
	replacement := httptest.NewRequest(http.MethodGet, "http://example.com/replacement", nil)
	replacement.Header.Set("Origin", "https://client.example")
	auth := newExecutorRequestPlugin("jwt-auth", 1, func(
		http.ResponseWriter,
		*http.Request,
	) base.RequestPhaseResult {
		return base.ContinueRequest(replacement)
	})
	var terminalRequest *http.Request
	handler := NewRequestPipeline([]Binding{
		pipelineBinding("cors", cors, ScopeRoute, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, nil).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		terminalRequest = r
	}))
	request := httptest.NewRequest(http.MethodGet, "http://example.com/original", nil)
	request.Header.Set("Origin", "https://client.example")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if terminalRequest != replacement {
		t.Fatalf("terminal request = %p, want auth replacement %p", terminalRequest, replacement)
	}
}

func TestRequestMiddlewarePreservesInnerPluginPanicIdentity(t *testing.T) {
	want := &PanicError{
		Factory: "inner",
		Phase:   PhaseAccess,
		Value:   "inner panic",
		Stack:   []byte("inner stack"),
	}
	plugin := newExecutorRequestPlugin("request-id", 1, func(
		http.ResponseWriter,
		*http.Request,
	) base.RequestPhaseResult {
		panic(want)
	})
	handler := NewRequestPipeline([]Binding{
		pipelineBinding("request-id", plugin, ScopeRoute, 1),
	}, nil).Then(http.NotFoundHandler())
	if got := recoverHandlerPanic(t, handler); got != want {
		t.Fatalf("panic = %#v, want original %#v", got, want)
	}
}

func TestRequestMiddlewareDoesNotRelabelTerminalPanic(t *testing.T) {
	want := &struct{ owner string }{owner: "core"}
	plugin := newExecutorRequestPlugin("request-id", 1, func(
		_ http.ResponseWriter,
		r *http.Request,
	) base.RequestPhaseResult {
		return base.ContinueRequest(r)
	})
	handler := NewRequestPipeline([]Binding{
		pipelineBinding("request-id", plugin, ScopeRoute, 1),
	}, nil).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) }))
	if got := recoverHandlerPanic(t, handler); got != want {
		t.Fatalf("panic = %#v, want original %#v", got, want)
	}
}

func TestCompositeChildPanicUsesOuterBindingIdentity(t *testing.T) {
	want := &struct{ child string }{child: "key-auth"}
	multiAuth := newExecutorRequestPlugin("multi-auth", 1, func(
		http.ResponseWriter,
		*http.Request,
	) base.RequestPhaseResult {
		func() { panic(want) }()
		return base.RequestPhaseResult{}
	})
	handler := NewRequestPipeline([]Binding{
		pipelineBinding("multi-auth", multiAuth, ScopeRoute, 1),
	}, nil).Then(http.NotFoundHandler())
	recovered := recoverHandlerPanic(t, handler)
	panicErr, ok := recovered.(*PanicError)
	if !ok {
		t.Fatalf("panic = %T, want *PanicError", recovered)
	}
	if panicErr.Factory != "multi-auth" || panicErr.Phase != PhaseAccess || panicErr.Value != want {
		t.Fatalf("panic metadata = %#v", panicErr)
	}
}

func TestBeforeProxyPluginPanicUsesRegistrationIdentity(t *testing.T) {
	want := &struct{ callback string }{callback: "mirror"}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = apisixctx.WithBeforeProxyHookRegistration(request, apisixctx.BeforeProxyHookRegistration{
		Owner: "proxy-mirror",
		Phase: string(PhaseBeforeProxy),
		Hook:  func(*http.Request) error { panic(want) },
	})
	handler := NewRequestPipeline(nil, nil).Then(http.NotFoundHandler())
	recovered := recoverHandlerPanicWithRequest(t, handler, request)
	panicErr, ok := recovered.(*PanicError)
	if !ok {
		t.Fatalf("panic = %T, want *PanicError", recovered)
	}
	if panicErr.Factory != "proxy-mirror" || panicErr.Phase != PhaseBeforeProxy ||
		panicErr.Value != want || len(panicErr.Stack) == 0 {
		t.Fatalf("panic metadata = %#v", panicErr)
	}
}

func TestBeforeProxyInvalidRegistrationReturnsStable500(t *testing.T) {
	tests := []apisixctx.BeforeProxyHookRegistration{
		{Owner: "not-registered", Phase: string(PhaseBeforeProxy), Hook: func(*http.Request) error { return nil }},
		{Owner: "proxy-mirror", Phase: "not-a-phase", Hook: func(*http.Request) error { return nil }},
	}
	for _, registration := range tests {
		t.Run(registration.Owner+":"+registration.Phase, func(t *testing.T) {
			request := apisixctx.WithBeforeProxyHookRegistration(
				httptest.NewRequest(http.MethodGet, "/", nil),
				registration,
			)
			response := httptest.NewRecorder()
			NewRequestPipeline(nil, nil).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("terminal called for invalid before-proxy registration")
			})).ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError ||
				response.Body.String() != `{"message":"Internal Server Error"}` {
				t.Fatalf("response = %d/%q, want stable 500", response.Code, response.Body.String())
			}
		})
	}
}

func TestBeforeProxyCompatibilityHookPanicRemainsRaw(t *testing.T) {
	want := &struct{ owner string }{owner: "core"}
	request := apisixctx.WithBeforeProxyHook(
		httptest.NewRequest(http.MethodGet, "/", nil),
		func(*http.Request) error { panic(want) },
	)
	handler := NewRequestPipeline(nil, nil).Then(http.NotFoundHandler())
	for range 2 {
		if recovered := recoverHandlerPanicWithRequest(t, handler, request); recovered != want {
			t.Fatalf("panic = %#v, want raw core panic %#v", recovered, want)
		}
	}
}

func recoverHandlerPanicWithRequest(
	t *testing.T,
	handler http.Handler,
	request *http.Request,
) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	handler.ServeHTTP(httptest.NewRecorder(), request)
	t.Fatal("handler did not panic")
	return nil
}

func TestRequestPipelineMapsOversizedBeforeProxyBodyTo413(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456"))
	request = apisixctx.WithBeforeProxyHook(request, func(*http.Request) error {
		return &base.BodyTooLargeError{Limit: 5}
	})
	terminalCalled := false
	response := httptest.NewRecorder()
	NewRequestPipeline(nil, nil).Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		terminalCalled = true
	})).ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge || terminalCalled {
		t.Fatalf("status=%d terminal=%t, want 413 before terminal", response.Code, terminalCalled)
	}
}

func TestRequestPipelineUsesCheckedBindingStageWithoutReresolving(t *testing.T) {
	config := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{RequestStage: "access"}}
	plugin := newResponseTestPlugin("serverless-pre-function", 1, config)
	calls := 0
	plugin.request = func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
		calls++
		return base.ContinueRequest(r)
	}
	binding := checkedResponseBinding(t, "serverless-pre-function", plugin, ScopeRoute, "route")
	config.fail.Store(true)
	NewRequestPipeline([]Binding{binding}, nil).Then(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if calls != 1 {
		t.Fatalf("request phase calls = %d, want 1", calls)
	}
}

func TestPostResolutionHookRunsAfterWinnerMergeBeforeAnyLaterStage(t *testing.T) {
	order := make([]string, 0, 7)
	phase := func(marker string) func(http.ResponseWriter, *http.Request) base.RequestPhaseResult {
		return func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			order = append(order, marker)
			return base.ContinueRequest(r)
		}
	}
	auth := newExecutorRequestPlugin("auth", 500, phase("auth"))
	routeRewrite := newExecutorRequestPlugin("route-rewrite", 400, phase("route-loser"))
	consumerRewrite := newExecutorRequestPlugin("consumer-rewrite", 400, phase("consumer-rewrite"))
	access := newExecutorRequestPlugin("access", 300, phase("access"))
	before := newResponseTestPlugin(
		"serverless-pre-function",
		200,
		responseTestConfig{stage: "before_proxy"},
	)
	before.request = phase("before-proxy")
	beforeBinding := checkedResponseBinding(t, "serverless-pre-function", before, ScopeRoute, "route")
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("jwt-auth", auth, ScopeRoute, 500),
		pipelineBinding("proxy-rewrite", routeRewrite, ScopeRoute, 400),
		pipelineBinding("limit-conn", access, ScopeRoute, 300),
		beforeBinding,
	}, func(r *http.Request) (ConsumerResolution, error) {
		order = append(order, "resolver")
		return ConsumerResolution{
			Request:  r,
			Resolved: true,
			Bindings: []Binding{pipelineBinding("proxy-rewrite", consumerRewrite, ScopeConsumer, 400)},
		}, nil
	})
	pipeline.ThenWithPostResolutionHook(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "terminal") }),
		func(r *http.Request, effective EffectiveBindingSet) (*http.Request, error) {
			order = append(order, "hook")
			if len(effective.merged) != 4 || effective.merged[1].Descriptor.Factory != "proxy-rewrite" ||
				effective.merged[1].Plugin != consumerRewrite {
				t.Fatalf("effective winners = %#v", effective.merged)
			}
			return r, nil
		},
	).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	want := []string{"auth", "resolver", "hook", "consumer-rewrite", "access", "before-proxy", "terminal"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestAuthAndResolverStopsDoNotInvokePostResolutionHook(t *testing.T) {
	t.Run("authentication stop", func(t *testing.T) {
		auth := newExecutorRequestPlugin(
			"auth",
			1,
			func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
				return base.StopRequest(r)
			},
		)
		hookCalls := 0
		NewRequestPipeline([]Binding{pipelineBinding("jwt-auth", auth, ScopeRoute, 1)}, nil).
			ThenWithPostResolutionHook(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
				func(r *http.Request, _ EffectiveBindingSet) (*http.Request, error) {
					hookCalls++
					return r, nil
				},
			).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if hookCalls != 0 {
			t.Fatalf("hook calls = %d", hookCalls)
		}
	})

	t.Run("resolver stop", func(t *testing.T) {
		hookCalls := 0
		NewRequestPipeline(nil, func(*http.Request) (ConsumerResolution, error) {
			return ConsumerResolution{}, errors.New("resolver failed")
		}).ThenWithPostResolutionHook(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("terminal called") }),
			func(r *http.Request, _ EffectiveBindingSet) (*http.Request, error) {
				hookCalls++
				return r, nil
			},
		).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if hookCalls != 0 {
			t.Fatalf("hook calls = %d", hookCalls)
		}
	})
}

func TestEffectiveBindingSetClonePreservesPrivateFactoryStageScopeProvenance(t *testing.T) {
	global := pipelineBinding("proxy-cache", newExecutorRequestPlugin("global", 2, nil), ScopeGlobal, 2)
	merged := pipelineBinding("body-transformer", newExecutorRequestPlugin("merged", 1, nil), ScopeRoute, 1)
	set := EffectiveBindingSet{global: []Binding{global}, merged: []Binding{merged}}
	clone := cloneEffectiveBindingSet(set)
	set.global[0].Descriptor.Factory = "mutated"
	set.global[0].Descriptor.Phases[0] = PhaseLog
	set.global[0].Descriptor.Scopes[0] = ScopeConsumer
	set.merged[0].Provenance.ID = "mutated"
	if clone.global[0].Descriptor.Factory != "proxy-cache" ||
		clone.global[0].Descriptor.Phases[0] == PhaseLog ||
		clone.global[0].Descriptor.Scopes[0] == ScopeConsumer ||
		clone.global[0].Descriptor.RequestStage() != global.Descriptor.RequestStage() ||
		clone.global[0].Scope != ScopeGlobal ||
		clone.merged[0].Provenance.ID == "mutated" {
		t.Fatalf("clone = %#v", clone)
	}
}

func newExecutorCORSPlugin(t *testing.T, config corsplugin.Config) *corsplugin.Plugin {
	t.Helper()
	plugin := &corsplugin.Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("CORS Init() error = %v", err)
	}
	configured, ok := plugin.Config().(*corsplugin.Config)
	if !ok {
		t.Fatalf("CORS Config() type = %T, want *cors.Config", plugin.Config())
	}
	*configured = config
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("CORS PostInit() error = %v", err)
	}
	return plugin
}

func countExecutorVaryToken(header http.Header, want string) int {
	count := 0
	for _, value := range header.Values("Vary") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				count++
			}
		}
	}
	return count
}

func TestProvisionalResponseWriterCommitsHeadersOnEarlyResponse(t *testing.T) {
	response := httptest.NewRecorder()
	writer := provisionalResponseWriter(response)
	writer.Header().Set("Access-Control-Allow-Origin", "https://client.example")
	writer.WriteHeader(http.StatusUnauthorized)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want 401", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("response CORS origin = %q, want client origin", got)
	}
}

func TestRequestPipelineCORSAddsHeadersToAuthenticationRejection(t *testing.T) {
	cors := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins:  "https://client.example",
		AllowMethods:  http.MethodGet,
		AllowHeaders:  "Authorization",
		ExposeHeaders: "X-Request-ID",
	})
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized"))
			return base.StopRequest(r)
		},
	)
	resolverCalls := 0
	terminalCalls := 0
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("cors", cors, ScopeRoute, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, func(r *http.Request) (ConsumerResolution, error) {
		resolverCalls++
		return ConsumerResolution{Request: r, Resolved: true}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		terminalCalls++
	})).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized || response.Body.String() != "unauthorized" {
		t.Fatalf("authentication response = %d/%q, want 401/unauthorized", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want client origin", got)
	}
	if got := countExecutorVaryToken(response.Header(), "Origin"); got != 1 {
		t.Fatalf("Vary: Origin count = %d, want 1 (headers=%v)", got, response.Header().Values("Vary"))
	}
	if resolverCalls != 0 || terminalCalls != 0 {
		t.Fatalf("resolver/terminal calls = %d/%d, want 0/0", resolverCalls, terminalCalls)
	}
}

func TestRequestPipelineRouteCORSOwnsAuthenticationRejectionAcrossStaticScopes(t *testing.T) {
	routeCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
		AllowHeaders: "X-Route",
	})
	globalCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
		AllowHeaders: "X-Global",
	})
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.WriteHeader(http.StatusUnauthorized)
			return base.StopRequest(r)
		},
	)
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("cors", routeCORS, ScopeRoute, 4000),
		pipelineBinding("cors", globalCORS, ScopeGlobal, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, nil)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("terminal called after authentication rejection")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("authentication status = %d, want 401", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Headers"); got != "X-Route" {
		t.Fatalf("authentication CORS allow headers = %q, want route policy X-Route", got)
	}
}

func TestRequestPipelineCORSPreflightRunsBeforeAuthentication(t *testing.T) {
	cors := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
	})
	authCalls := 0
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			authCalls++
			w.WriteHeader(http.StatusUnauthorized)
			return base.StopRequest(r)
		},
	)
	terminalCalls := 0
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("cors", cors, ScopeRoute, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, nil)
	request := httptest.NewRequest(http.MethodOptions, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		terminalCalls++
	})).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("preflight Access-Control-Allow-Origin = %q, want client origin", got)
	}
	if authCalls != 0 || terminalCalls != 0 {
		t.Fatalf("auth/terminal calls = %d/%d, want 0/0", authCalls, terminalCalls)
	}
}

func TestRequestPipelineRouteCORSOwnsPreflightAcrossStaticScopes(t *testing.T) {
	routeCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
		AllowHeaders: "X-Route",
	})
	globalCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
		AllowHeaders: "X-Global",
	})
	authCalls := 0
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			authCalls++
			w.WriteHeader(http.StatusUnauthorized)
			return base.StopRequest(r)
		},
	)
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("cors", routeCORS, ScopeRoute, 4000),
		pipelineBinding("cors", globalCORS, ScopeGlobal, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, nil)
	request := httptest.NewRequest(http.MethodOptions, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodGet)
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("terminal called after CORS preflight")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Headers"); got != "X-Route" {
		t.Fatalf("preflight CORS allow headers = %q, want route policy X-Route", got)
	}
	if authCalls != 0 {
		t.Fatalf("authentication calls = %d, want 0", authCalls)
	}
}

func TestRequestPipelineCORSNormalizesSuccessfulResponseAfterStreamingFilter(t *testing.T) {
	cors := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
		AllowMethods: http.MethodGet,
	})
	corsBinding := pipelineBinding("cors", cors, ScopeRoute, 4000)
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.Header().Set("X-UserId", "u-1")
			w.Header().Set("X-Username", "alice")
			w.Header().Set("X-Nickname", "Alice")
			return base.ContinueRequest(r)
		},
	)
	streaming, err := NewStreamingResponseExecutor([]Binding{corsBinding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	terminalCalls := 0
	pipeline := NewRequestPipeline([]Binding{
		corsBinding,
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, nil).WithStreamingResponseExecutor(streaming)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		terminalCalls++
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || terminalCalls != 1 {
		t.Fatalf("successful response = %d, terminal calls = %d, want 204/1", response.Code, terminalCalls)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("successful Access-Control-Allow-Origin = %q, want client origin", got)
	}
	if got := countExecutorVaryToken(response.Header(), "Origin"); got != 1 {
		t.Fatalf("successful Vary: Origin count = %d, want 1 (headers=%v)", got, response.Header().Values("Vary"))
	}
	for name, want := range map[string]string{
		"X-UserId": "u-1", "X-Username": "alice", "X-Nickname": "Alice",
	} {
		if got := response.Header().Get(name); got != want {
			t.Fatalf("successful authentication header %s = %q, want %q", name, got, want)
		}
	}
}

func TestRequestPipelineCORSDoesNotApplyConsumerBindingBeforeAuthentication(t *testing.T) {
	routeCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://route.example",
	})
	consumerCORS := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://consumer.example",
	})
	auth := newExecutorRequestPlugin(
		"auth",
		1,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.WriteHeader(http.StatusUnauthorized)
			return base.StopRequest(r)
		},
	)
	resolverCalls := 0
	pipeline := NewRequestPipeline([]Binding{
		pipelineBinding("cors", routeCORS, ScopeRoute, 4000),
		pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
	}, func(r *http.Request) (ConsumerResolution, error) {
		resolverCalls++
		return ConsumerResolution{
			Request:  r,
			Resolved: true,
			Bindings: []Binding{pipelineBinding("cors", consumerCORS, ScopeConsumer, 4000)},
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://route.example")
	response := httptest.NewRecorder()
	pipeline.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("terminal called after authentication rejection")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("authentication status = %d, want 401", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://route.example" {
		t.Fatalf("route CORS origin = %q, want route origin", got)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver calls = %d, want 0 before authentication succeeds", resolverCalls)
	}
}
