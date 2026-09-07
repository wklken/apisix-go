package plugin

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestConsumerOverrideDoesNotRerunExistingProxyRewrite(t *testing.T) {
	route := parityBinding(t, "proxy-rewrite", `{"headers":{"set":{"X-Who":"route"}}}`, ScopeRoute)
	consumer := parityBinding(t, "proxy-rewrite", `{"headers":{"set":{"X-Who":"consumer"}}}`, ScopeConsumer)
	auth := parityBinding(t, "key-auth", `{"anonymous_consumer":"r5"}`, ScopeRoute)
	auth.Plugin.(interface{ SetDependencies(base.Dependencies) }).SetDependencies(
		base.Dependencies{Consumers: parityConsumerLookup{}},
	)
	handler := NewRequestPipeline([]Binding{auth, route}, func(r *http.Request) (ConsumerResolution, error) {
		if _, ok := apisixctx.AuthenticationStateFrom(r); !ok {
			t.Fatal("key-auth did not authenticate")
		}
		return ConsumerResolution{
			Bindings:          []Binding{consumer},
			OverrideFactories: []string{"proxy-rewrite"},
			Request:           r,
			Resolved:          true,
		}, nil
	}).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Observed-Who", r.Header.Get("X-Who"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Header.Get("X-Who")))
	}))
	r, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/override-consumer-rewrite", nil),
		time.Now(),
	)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	t.Logf("status=%d body=%q observed=%q", w.Code, w.Body.String(), w.Header().Get("X-Observed-Who"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "route" {
		t.Fatalf("X-Who after executor = %q, want route", got)
	}
}

func TestNewConsumerRewriteStillRuns(t *testing.T) {
	consumer := parityBinding(t, "proxy-rewrite", `{"headers":{"set":{"X-Who":"consumer"}}}`, ScopeConsumer)
	auth := parityBinding(t, "key-auth", `{"anonymous_consumer":"r5"}`, ScopeRoute)
	auth.Plugin.(interface{ SetDependencies(base.Dependencies) }).SetDependencies(
		base.Dependencies{Consumers: parityConsumerLookup{}},
	)
	handler := NewRequestPipeline([]Binding{auth}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{
			Bindings:          []Binding{consumer},
			OverrideFactories: []string{"proxy-rewrite"},
			Request:           r,
			Resolved:          true,
		}, nil
	}).Then(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(r.Header.Get("X-Who")))
	}))
	r, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/new-consumer-rewrite", nil),
		time.Now(),
	)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "consumer" {
		t.Fatalf("new consumer rewrite status=%d body=%q, want 200/consumer", w.Code, w.Body.String())
	}
}

func TestConsumerRewriteSkipsFactoryPresentInAnotherRoutePhase(t *testing.T) {
	var calls []string
	makeBinding := func(scope Scope, stage, marker string) Binding {
		p := newResponseTestPlugin("serverless-pre-function", 1, responseTestConfig{stage: stage})
		p.request = func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			calls = append(calls, marker)
			return base.ContinueRequest(r)
		}
		return checkedResponseBinding(t, "serverless-pre-function", p, scope, marker)
	}
	route := makeBinding(ScopeRoute, "access", "route")
	consumer := makeBinding(ScopeConsumer, "rewrite", "consumer")
	NewRequestPipeline([]Binding{route}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Bindings: []Binding{consumer}, Request: r, Resolved: true}, nil
	}).Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if len(calls) != 0 {
		t.Fatalf("phase callbacks=%v; existing factory must not gain consumer rewrite", calls)
	}
}
