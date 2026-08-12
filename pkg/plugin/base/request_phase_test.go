package base

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

type requestPhaseTestPlugin struct {
	result RequestPhaseResult
	run    func(http.ResponseWriter, *http.Request) RequestPhaseResult
	calls  atomic.Int32
}

func (p *requestPhaseTestPlugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) RequestPhaseResult {
	p.calls.Add(1)
	if p.run != nil {
		return p.run(w, r)
	}
	return p.result
}

func TestRequestPhaseResultConstructors(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := ContinueRequest(request); got != (RequestPhaseResult{
		Request:  request,
		Decision: RequestContinue,
		Source:   apisixctx.ResponseSourceUnknown,
	}) {
		t.Fatalf("ContinueRequest() = %#v", got)
	}
	if got := StopRequest(request); got != (RequestPhaseResult{
		Request:  request,
		Decision: RequestStop,
		Source:   apisixctx.ResponseSourceEarlyStop,
	}) {
		t.Fatalf("StopRequest() = %#v", got)
	}
	if got := StopRequestWithSource(request, apisixctx.ResponseSourceCacheHit); got != (RequestPhaseResult{
		Request:  request,
		Decision: RequestStop,
		Source:   apisixctx.ResponseSourceCacheHit,
	}) {
		t.Fatalf("StopRequestWithSource() = %#v", got)
	}
}

func TestAdaptRequestPhasePropagatesReplacement(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	replacement := request.WithContext(request.Context())
	replacement.Header.Set("X-Replacement", "yes")
	plugin := &requestPhaseTestPlugin{result: ContinueRequest(replacement)}
	var got *http.Request
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })

	AdaptRequestPhase(plugin, next).ServeHTTP(httptest.NewRecorder(), request)
	if plugin.calls.Load() != 1 {
		t.Fatalf("RunRequestPhase calls = %d, want 1", plugin.calls.Load())
	}
	if got != replacement || got.Header.Get("X-Replacement") != "yes" {
		t.Fatalf("next request = %p/%q, want replacement %p/yes", got, got.Header.Get("X-Replacement"), replacement)
	}
	if lifecycle.FinalRequest() != replacement {
		t.Fatalf("FinalRequest() = %p, want replacement %p", lifecycle.FinalRequest(), replacement)
	}
}

func TestRequestPhaseAdapterRetainsNilRequest(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{result: ContinueRequest(nil)}
	var got *http.Request
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })

	AdaptRequestPhase(plugin, next).ServeHTTP(httptest.NewRecorder(), request)
	if got != request {
		t.Fatalf("next request = %p, want input %p", got, request)
	}
	if lifecycle.FinalRequest() != request {
		t.Fatalf("FinalRequest() = %p, want input %p", lifecycle.FinalRequest(), request)
	}
}

func TestAdaptRequestPhaseRecordsEarlyStopSource(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{result: StopRequest(nil)}
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })

	AdaptRequestPhase(plugin, next).ServeHTTP(httptest.NewRecorder(), request)
	if called {
		t.Fatal("next called for stopped request")
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
	if lifecycle.FinalRequest() != request {
		t.Fatalf("FinalRequest() = %p, want input %p", lifecycle.FinalRequest(), request)
	}
}

func TestAdaptRequestPhaseStops(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	plugin := &requestPhaseTestPlugin{result: StopRequest(request)}
	called := false
	AdaptRequestPhase(plugin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if called {
		t.Fatal("next called for stopped request")
	}
}

func TestRequestPhaseAdapterRecordsExplicitSource(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{
		result: StopRequestWithSource(request, apisixctx.ResponseSourceCacheHit),
	}
	AdaptRequestPhase(plugin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next called for stopped request")
	})).ServeHTTP(httptest.NewRecorder(), request)
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceCacheHit {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceCacheHit)
	}
	if got := apisixctx.GetRequestVar(request, "$response_source"); got != string(apisixctx.ResponseSourceCacheHit) {
		t.Fatalf("$response_source request mirror = %#v", got)
	}
	if got := apisixctx.GetApisixVar(request, "$response_source"); got != string(apisixctx.ResponseSourceCacheHit) {
		t.Fatalf("$response_source APISIX mirror = %#v", got)
	}
}

func TestStopRequestPreservesAPISIXSource(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{result: StopRequestWithSource(request, apisixctx.ResponseSourceAPISIX)}
	AdaptRequestPhase(plugin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next called for stopped request")
	})).ServeHTTP(httptest.NewRecorder(), request)
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceAPISIX)
	}
}

func TestRequestPhaseAdapterNormalizesInvalidSource(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{result: StopRequestWithSource(request, apisixctx.ResponseSource("plugin-owned"))}
	AdaptRequestPhase(plugin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next called for stopped request")
	})).ServeHTTP(httptest.NewRecorder(), request)
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestAdaptRequestPhaseUnknownDecisionStops(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plugin := &requestPhaseTestPlugin{result: RequestPhaseResult{
		Request:  request,
		Decision: RequestDecision(99),
		Source:   apisixctx.ResponseSourceCacheHit,
	}}
	called := false
	AdaptRequestPhase(plugin, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if called {
		t.Fatal("next called for unknown decision")
	}
	if got := lifecycle.ResponseSource(); got != apisixctx.ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, apisixctx.ResponseSourceEarlyStop)
	}
}

func TestRequestPhaseAdapterDoesNotCreateLifecycle(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	plugin := &requestPhaseTestPlugin{result: ContinueRequest(nil)}
	var got *http.Request
	AdaptRequestPhase(plugin, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })).ServeHTTP(
		httptest.NewRecorder(),
		request,
	)
	if got != request {
		t.Fatalf("next request = %p, want input %p", got, request)
	}
	if lifecycle := apisixctx.GetRequestLifecycle(got); lifecycle != nil {
		t.Fatalf("adapter created lifecycle %p", lifecycle)
	}
}
