package ctx

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
	if failures[0].Kind != FinalizerOwnerPlugin {
		t.Fatalf("panic failure kind = %v, want plugin", failures[0].Kind)
	}
	if failures[1].Owner != "error" || !errors.Is(failures[1].Err, wantErr) || failures[1].PanicValue != nil {
		t.Fatalf("error failure = %#v", failures[1])
	}
	if result := lifecycle.FinalizeResult(); result.FatalPanic != nil {
		t.Fatalf("plugin panic became fatal: %#v", result.FatalPanic)
	}
}

func TestRequestLifecycleCoreFinalizerPanicRunsRemainingFinalizers(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	order := make([]string, 0, 3)
	lifecycle.AddFinalizer("plugin-last", func() error {
		order = append(order, "plugin-last")
		return nil
	})
	lifecycle.AddCoreInvariantFinalizer("core", func() error {
		order = append(order, "core")
		panic("core-finalizer")
	})
	lifecycle.AddFinalizer("plugin-first", func() error {
		order = append(order, "plugin-first")
		panic("plugin-finalizer")
	})

	result := lifecycle.FinalizeResult()
	if !reflect.DeepEqual(order, []string{"plugin-first", "core", "plugin-last"}) {
		t.Fatalf("finalizer order = %v", order)
	}
	if result.FatalPanic == nil || result.FatalPanic.PanicValue != "core-finalizer" {
		t.Fatalf("result = %#v", result)
	}
	if result.FatalPanic.Kind != FinalizerOwnerCoreInvariant {
		t.Fatalf("fatal panic kind = %v, want core invariant", result.FatalPanic.Kind)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("failures = %#v", result.Failures)
	}
	if result.Failures[0].Owner != "plugin-first" || result.Failures[0].Kind != FinalizerOwnerPlugin {
		t.Fatalf("plugin failure = %#v", result.Failures[0])
	}
	if result.Failures[1].Owner != "core" || result.Failures[1].Kind != FinalizerOwnerCoreInvariant {
		t.Fatalf("core failure = %#v", result.Failures[1])
	}
}

func TestRequestLifecycleFirstCorePanicInExecutionOrderIsFatal(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	wantErr := errors.New("core error")
	lifecycle.AddCoreInvariantFinalizer("core-panic-second", func() error { panic("second") })
	lifecycle.AddCoreInvariantFinalizer("core-error", func() error { return wantErr })
	lifecycle.AddCoreInvariantFinalizer("core-panic-first", func() error { panic("first") })

	result := lifecycle.FinalizeResult()
	if result.FatalPanic == nil || result.FatalPanic.Owner != "core-panic-first" ||
		result.FatalPanic.PanicValue != "first" {
		t.Fatalf("fatal panic = %#v", result.FatalPanic)
	}
	if len(result.Failures) != 3 {
		t.Fatalf("failures = %#v", result.Failures)
	}
	if result.Failures[0].Owner != "core-panic-first" ||
		result.Failures[1].Owner != "core-error" ||
		result.Failures[2].Owner != "core-panic-second" {
		t.Fatalf("failure order = %#v", result.Failures)
	}
	if !errors.Is(result.Failures[1].Err, wantErr) {
		t.Fatalf("core error failure = %#v", result.Failures[1])
	}
}

func TestRequestLifecycleFinalizeMethodsShareDetachedExactlyOnceResult(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	wantErr := errors.New("plugin error")
	wantPanic := &struct{ label string }{label: "core panic"}
	var calls atomic.Int32
	lifecycle.AddFinalizer("plugin", func() error {
		calls.Add(1)
		return wantErr
	})
	lifecycle.AddCoreInvariantFinalizer("core", func() error {
		calls.Add(1)
		panic(wantPanic)
	})

	const callers = 16
	results := make(chan FinalizationResult, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			if i%2 == 0 {
				results <- lifecycle.FinalizeResult()
				return
			}
			results <- FinalizationResult{Failures: lifecycle.Finalize()}
		})
	}
	wg.Wait()
	close(results)

	if calls.Load() != 2 {
		t.Fatalf("finalizer calls = %d, want 2", calls.Load())
	}
	for result := range results {
		if len(result.Failures) != 2 {
			t.Fatalf("failures = %#v", result.Failures)
		}
		if result.Failures[0].Owner != "core" || result.Failures[0].PanicValue != wantPanic {
			t.Fatalf("core failure = %#v", result.Failures[0])
		}
		if result.Failures[1].Owner != "plugin" || result.Failures[1].Err != wantErr {
			t.Fatalf("plugin failure = %#v", result.Failures[1])
		}
		if result.FatalPanic != nil && result.FatalPanic.PanicValue != wantPanic {
			t.Fatalf("fatal panic = %#v", result.FatalPanic)
		}
	}

	first := lifecycle.FinalizeResult()
	first.Failures[0].Owner = "mutated"
	first.Failures[0].Stack[0] ^= 0xff
	first.FatalPanic.Owner = "mutated-fatal"
	first.FatalPanic.Stack[0] ^= 0xff
	second := lifecycle.FinalizeResult()
	if second.Failures[0].Owner != "core" || second.FatalPanic.Owner != "core" {
		t.Fatalf("cached result was mutated: %#v", second)
	}
	if second.Failures[0].Stack[0] == first.Failures[0].Stack[0] ||
		second.FatalPanic.Stack[0] == first.FatalPanic.Stack[0] {
		t.Fatalf("returned stacks share backing storage: first=%#v second=%#v", first, second)
	}
	if second.Failures[0].PanicValue != wantPanic || second.Failures[1].Err != wantErr {
		t.Fatalf("arbitrary failure identity changed: %#v", second)
	}
	legacy := lifecycle.Finalize()
	legacy[0].Owner = "mutated-legacy"
	legacy[0].Stack[0] ^= 0xff
	legacyAgain := lifecycle.Finalize()
	if legacyAgain[0].Owner != "core" || legacyAgain[0].Stack[0] == legacy[0].Stack[0] {
		t.Fatalf("Finalize returned shared failure data: first=%#v second=%#v", legacy, legacyAgain)
	}
}

func TestRequestLifecycleAcceptsFinalizersBeforeAndRejectsAfterFinalize(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Now())
	var called atomic.Int32
	if !lifecycle.AddFinalizer("plugin", func() error { called.Add(1); return nil }) {
		t.Fatal("AddFinalizer before finalization returned false")
	}
	if !lifecycle.AddCoreInvariantFinalizer("core", func() error { called.Add(1); return nil }) {
		t.Fatal("AddCoreInvariantFinalizer before finalization returned false")
	}
	lifecycle.FinalizeResult()
	if lifecycle.AddFinalizer("late-plugin", func() error { called.Add(1); return nil }) {
		t.Fatal("late AddFinalizer returned true")
	}
	if lifecycle.AddCoreInvariantFinalizer("late-core", func() error { called.Add(1); return nil }) {
		t.Fatal("late AddCoreInvariantFinalizer returned true")
	}
	if called.Load() != 2 {
		t.Fatalf("finalizer calls = %d, want 2", called.Load())
	}
}

func TestRequestLifecycleCoreFinalizerRegistrationTrustBoundary(t *testing.T) {
	fixture := map[string][]byte{
		"pkg/plugin/rogue.go": []byte(`package plugin
func Rogue(l lifecycle) {
	callback := func() { l.AddCoreInvariantFinalizer("rogue", nil) }
	callback()
}`),
	}
	wantFixtureSites := []callSite{{File: "pkg/plugin/rogue.go", Function: "Rogue"}}
	if got := findSelectorCalls(t, fixture, "AddCoreInvariantFinalizer"); !reflect.DeepEqual(got, wantFixtureSites) {
		t.Fatalf("selector call sites = %#v", got)
	}

	pluginSources := pluginProductionSources(t)
	sites := findSelectorCalls(t, pluginSources, "AddCoreInvariantFinalizer")
	allowed := callSite{File: "pkg/plugin/log_executor.go", Function: "RegisterComposite"}
	for _, site := range sites {
		if site != allowed {
			t.Fatalf("unauthorized core finalizer registration: %#v", site)
		}
	}
}

type callSite struct {
	File     string
	Function string
}

func pluginProductionSources(t *testing.T) map[string][]byte {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate lifecycle test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../../.."))
	pluginRoot := filepath.Join(repoRoot, "pkg/plugin")
	sources := make(map[string][]byte)
	err := filepath.WalkDir(pluginRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(relative)] = source
		return nil
	})
	if err != nil {
		t.Fatalf("walk plugin production sources: %v", err)
	}
	return sources
}

func findSelectorCalls(t *testing.T, sources map[string][]byte, selectorName string) []callSite {
	t.Helper()
	fset := token.NewFileSet()
	var sites []callSite
	for relative, source := range sources {
		parsed, err := parser.ParseFile(fset, relative, source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", relative, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == selectorName {
					sites = append(sites, callSite{File: relative, Function: function.Name.Name})
				}
				return true
			})
		}
	}
	return sites
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

func TestRequestLifecycleCompletePublishesOutcomeAndFinishedAt(t *testing.T) {
	lifecycle := NewRequestLifecycle(time.Unix(10, 0))
	finished := time.Unix(20, 123)
	want := ResponseOutcome{
		Kind:      RequestOutcomeRecoveredPanic,
		Status:    http.StatusInternalServerError,
		Bytes:     17,
		Committed: true,
	}
	lifecycle.Complete(want, finished)
	if got := lifecycle.Outcome(); got != want {
		t.Fatalf("Outcome() = %#v, want %#v", got, want)
	}
	if got := lifecycle.FinishedAt(); !got.Equal(finished) {
		t.Fatalf("FinishedAt() = %v, want %v", got, finished)
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
