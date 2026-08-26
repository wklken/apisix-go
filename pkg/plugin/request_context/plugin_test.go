package request_context

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestPostInitRequiresEffectiveConfigAndUsesConfiguredID(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.PostInit(); err == nil || err.Error() != "request-context: effective config is required" {
		t.Fatalf("PostInit() error = %v", err)
	}
	plugin.SetDependencies(base.Dependencies{Config: &config.EffectiveConfig{
		Config: config.Config{Apisix: config.Apisix{ID: "node-test"}},
	}})
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if plugin.nodeID != "node-test" {
		t.Fatalf("nodeID = %q, want node-test", plugin.nodeID)
	}
}

func TestRequestContextInitializesVariablesWithoutOwningRouteMetrics(t *testing.T) {
	startedAt := time.Now()
	lifecycle := apisixctx.NewRequestLifecycle(startedAt)
	request := apisixctx.WithRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		lifecycle,
	)
	request, _ = apisixctx.EnsureRequestLifecycle(request, startedAt)
	var state *apisixctx.RequestState

	handler := (&Plugin{config: Config{
		RouteID:     "route-1",
		RouteName:   "route-name",
		MatchedURI:  "/orders/:id",
		MatchedHost: "api.example.com",
		ServiceID:   "service-1",
		ServiceName: "service-name",
	}}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state = apisixctx.GetRequestState(r)
		if got := apisixctx.GetApisixVar(r, "$route_id"); got != "route-1" {
			t.Fatalf("$route_id = %q, want route-1", got)
		}
		if got := apisixctx.GetApisixVar(r, "$service_name"); got != "service-name" {
			t.Fatalf("$service_name = %q, want service-name", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if state == nil || state.ApisixVars == nil || state.RequestVars == nil {
		t.Fatal("request state was recycled before the outer lifecycle finalized")
	}
	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeCompleted,
		Status:    http.StatusNoContent,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v, want none", failures)
	}
	if state.ApisixVars == nil || state.RequestVars == nil {
		t.Fatal("request-context recycled state before the outer owner")
	}

	apisixctx.RecycleVars(request)
	if state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("outer lifecycle owner did not recycle request state")
	}
}

func TestRequestContextDirectHandlerRecyclesLifecycleState(t *testing.T) {
	var seen *http.Request
	handler := (&Plugin{config: Config{RouteID: "legacy-route"}}).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
	if seen == nil {
		t.Fatal("direct handler did not pass a request downstream")
	}
	if apisixctx.GetRequestLifecycle(seen) == nil {
		t.Fatal("direct handler did not create a local request lifecycle")
	}
	state := apisixctx.GetRequestState(seen)
	if state == nil || state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("direct handler did not recycle request state after finalization")
	}
}

func TestRequestContextDirectHandlerRecyclesLifecycleStateAfterPanic(t *testing.T) {
	var seen *http.Request
	handler := (&Plugin{}).Handler(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = r
		panic("downstream panic")
	}))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		)
	}()
	if recovered != "downstream panic" {
		t.Fatalf("recovered = %v, want downstream panic", recovered)
	}
	if seen == nil {
		t.Fatal("panic path did not pass a request downstream")
	}
	state := apisixctx.GetRequestState(seen)
	if state == nil || state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("direct panic path did not recycle request state")
	}
}

func TestRequestPhaseInitializesStateWithoutFinalizer(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Now(),
	)
	p := &Plugin{config: Config{RouteID: "phase-route"}}
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("request phase decision = %d, want continue", result.Decision)
	}
	if result.Request == nil || apisixctx.GetRequestState(result.Request) == nil {
		t.Fatal("request phase did not initialize shared request state")
	}
	if _, ok := any(p).(base.SnapshotFinalizerPlugin); ok {
		t.Fatal("request-context unexpectedly owns a snapshot finalizer")
	}
	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeCompleted,
		Status:    http.StatusNoContent,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v", failures)
	}
	apisixctx.RecycleVars(result.Request)
}
