package ctx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequestLifecycleFinalizesInReverseOrderExactlyOnce(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Unix(10, 0))
	var callsMu sync.Mutex
	var calls []string
	var count atomic.Int32
	for _, owner := range []string{"first", "second", "third"} {
		if !lifecycle.AddFinalizer(owner, func() error {
			count.Add(1)
			callsMu.Lock()
			defer callsMu.Unlock()
			calls = append(calls, owner)
			return nil
		}) {
			t.Fatalf("AddFinalizer(%q) = false", owner)
		}
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if failures := lifecycle.Finalize(); len(failures) != 0 {
				t.Errorf("Finalize() failures = %v", failures)
			}
		})
	}
	wg.Wait()

	if count.Load() != 3 {
		t.Fatalf("finalizer calls = %d, want 3", count.Load())
	}
	if !reflect.DeepEqual(calls, []string{"third", "second", "first"}) {
		t.Fatalf("finalizer order = %v", calls)
	}
	if lifecycle.StartedAt() != time.Unix(10, 0) {
		t.Fatalf("StartedAt() = %v", lifecycle.StartedAt())
	}
}

func TestRequestLifecycleCollectsErrorsAndPanicsAndContinues(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	wantErr := errors.New("finalizer failed")
	var calls []string
	add := func(owner string, fn RequestFinalizer) {
		t.Helper()
		if !lifecycle.AddFinalizer(owner, func() error {
			calls = append(calls, owner)
			return fn()
		}) {
			t.Fatalf("AddFinalizer(%q) = false", owner)
		}
	}
	add("first", func() error { return nil })
	add("error", func() error { return wantErr })
	add("panic", func() error { panic("boom") })
	add("last", func() error { return nil })

	failures := lifecycle.Finalize()
	if !reflect.DeepEqual(calls, []string{"last", "panic", "error", "first"}) {
		t.Fatalf("finalizer order = %v", calls)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %#v", failures)
	}
	if failures[0].Owner != "panic" || failures[0].PanicValue != "boom" || len(failures[0].Stack) == 0 {
		t.Fatalf("panic failure = %#v", failures[0])
	}
	if failures[1].Owner != "error" || !errors.Is(failures[1].Err, wantErr) || failures[1].PanicValue != nil {
		t.Fatalf("error failure = %#v", failures[1])
	}
}

func TestRequestLifecycleRejectsLateFinalizer(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	lifecycle.Finalize()
	called := false
	if lifecycle.AddFinalizer("late", func() error { called = true; return nil }) {
		t.Fatal("late AddFinalizer returned true")
	}
	if called {
		t.Fatal("late finalizer ran inline")
	}
}

func TestRequestLifecycleSharesOutcomeAcrossRequestCopies(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	lifecycle := NewRequestLifecycle(time.Now())
	request = WithRequestLifecycle(request, lifecycle)
	copy := request.Clone(request.Context())
	want := ResponseOutcome{Kind: RequestOutcomeRecoveredPanic, Status: 500, Bytes: 35, Committed: true}
	GetRequestLifecycle(copy).SetOutcome(want)
	if got := GetRequestLifecycle(request).Outcome(); got != want {
		t.Fatalf("Outcome() = %#v, want %#v", got, want)
	}
	ensured, gotLifecycle := EnsureRequestLifecycle(copy, time.Time{})
	if gotLifecycle != lifecycle || GetRequestState(ensured) == nil {
		t.Fatal("EnsureRequestLifecycle replaced the lifecycle or failed to initialize request state")
	}
	RecycleVars(ensured)
}

func TestRequestLifecycleInitializesSharedRequestState(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request, lifecycle := EnsureRequestLifecycle(request, time.Now())
	state := GetRequestState(request)
	if lifecycle == nil || state == nil || state.ApisixVars == nil || state.RequestVars == nil {
		t.Fatalf("lifecycle/state not initialized: lifecycle=%p state=%#v", lifecycle, state)
	}
	copy := request.WithContext(request.Context())
	if GetRequestState(copy) != state {
		t.Fatal("derived request does not share RequestState")
	}
	RegisterApisixVar(copy, "$route_id", "route-1")
	RegisterRequestVar(copy, "$custom", "value")
	if GetApisixVar(request, "$route_id") != "route-1" || GetRequestVar(request, "$custom") != "value" {
		t.Fatal("request maps drifted across request copies")
	}
	RecycleVars(request)
}

func TestRequestLifecycleInitializesAndTracksFinalRequestAndSource(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	ensured, lifecycle := EnsureRequestLifecycle(request, time.Now())
	if got := lifecycle.FinalRequest(); got != ensured {
		t.Fatalf("FinalRequest() = %p, want ensured request %p", got, ensured)
	}
	if got := lifecycle.ResponseSource(); got != ResponseSourceUnknown {
		t.Fatalf("ResponseSource() = %q, want %q", got, ResponseSourceUnknown)
	}

	replacement := ensured.WithContext(ensured.Context())
	lifecycle.SetFinalRequest(replacement)
	lifecycle.SetResponseSource(ResponseSourceCacheHit)
	if got := lifecycle.FinalRequest(); got != replacement {
		t.Fatalf("FinalRequest() = %p, want replacement %p", got, replacement)
	}
	if got := lifecycle.ResponseSource(); got != ResponseSourceCacheHit {
		t.Fatalf("ResponseSource() = %q, want %q", got, ResponseSourceCacheHit)
	}
	lifecycle.SetResponseSource(ResponseSource("plugin-owned"))
	if got := lifecycle.ResponseSource(); got != ResponseSourceUnknown {
		t.Fatalf("invalid ResponseSource() = %q, want %q", got, ResponseSourceUnknown)
	}
	RecycleVars(ensured)
}

func TestRequestLifecycleAcceptsAPISIXResponseSource(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	lifecycle.SetResponseSource(ResponseSourceAPISIX)
	if got := lifecycle.ResponseSource(); got != ResponseSourceAPISIX {
		t.Fatalf("ResponseSource() = %q, want %q", got, ResponseSourceAPISIX)
	}
}

func TestSetRequestResponseSourceSynchronizesLifecycleAndMirrors(t *testing.T) {
	request, lifecycle := EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	SetRequestResponseSource(request, ResponseSourceAPISIX)
	if got := lifecycle.ResponseSource(); got != ResponseSourceAPISIX {
		t.Fatalf("ResponseSource() = %q, want %q", got, ResponseSourceAPISIX)
	}
	if got := GetRequestVar(request, "$response_source"); got != string(ResponseSourceAPISIX) {
		t.Fatalf("request mirror = %#v", got)
	}
	if got := GetApisixVar(request, "$response_source"); got != string(ResponseSourceAPISIX) {
		t.Fatalf("APISIX mirror = %#v", got)
	}

	SetRequestResponseSource(request, ResponseSource("invalid"))
	if got := lifecycle.ResponseSource(); got != ResponseSourceUnknown {
		t.Fatalf("invalid source = %q, want unknown", got)
	}
	if got := GetRequestVar(request, "$response_source"); got != string(ResponseSourceUnknown) {
		t.Fatalf("invalid request mirror = %#v", got)
	}
}

func TestRequestLifecycleFinalRequestAndSourceConcurrentAccess(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	lifecycle := NewRequestLifecycle(time.Now())
	request = WithRequestLifecycle(request, lifecycle)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				lifecycle.SetFinalRequest(request)
				lifecycle.SetResponseSource(ResponseSourceEarlyStop)
				_ = lifecycle.FinalRequest()
				_ = lifecycle.ResponseSource()
			}
		})
	}
	wg.Wait()
	if got := lifecycle.FinalRequest(); got != request {
		t.Fatalf("FinalRequest() = %p, want %p", got, request)
	}
	if got := lifecycle.ResponseSource(); got != ResponseSourceEarlyStop {
		t.Fatalf("ResponseSource() = %q, want %q", got, ResponseSourceEarlyStop)
	}
}
