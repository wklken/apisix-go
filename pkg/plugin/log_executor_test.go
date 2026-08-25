package plugin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	keyauth "github.com/wklken/apisix-go/pkg/plugin/key_auth"
	requestcontext "github.com/wklken/apisix-go/pkg/plugin/request_context"
)

type logExecutorTestPlugin struct {
	base.BasePlugin
	mu             sync.Mutex
	order          *[]string
	panicLog       bool
	panicLogValue  any
	panicFinalizer any
	finalizer      bool
	seen           []base.LogSnapshot
}

type countingLogBody struct {
	reader    *strings.Reader
	bytesRead int
}

type failingLogBody struct {
	read bool
}

type logExecutorNoCallbackPlugin struct {
	base.BasePlugin
	config any
}

type logSanitizerTestPlugin struct {
	base.BasePlugin
	order      *[]string
	err        error
	panicValue any
}

type logSanitizerSelectorTestPlugin struct {
	logSanitizerTestPlugin
	selectSnapshot func(base.LogSnapshot) bool
	selectorSeen   *[]string
	selectorPanic  any
}

type logIdentityPolicyPlugin struct {
	base.BasePlugin
	name        string
	policy      base.LogCapturePolicy
	nameCalls   int
	policyCalls int
}

func (*logIdentityPolicyPlugin) Init() error                            { return nil }
func (*logIdentityPolicyPlugin) PostInit() error                        { return nil }
func (*logIdentityPolicyPlugin) Config() any                            { return nil }
func (*logIdentityPolicyPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *logIdentityPolicyPlugin) GetName() string {
	p.nameCalls++
	return p.name
}

func (p *logIdentityPolicyPlugin) LogCapturePolicy() base.LogCapturePolicy {
	p.policyCalls++
	return p.policy
}
func (*logIdentityPolicyPlugin) RunLogPhase(base.LogSnapshot) error { return nil }

func (p *logSanitizerTestPlugin) Init() error                            { return nil }
func (p *logSanitizerTestPlugin) PostInit() error                        { return nil }
func (p *logSanitizerTestPlugin) Config() any                            { return nil }
func (p *logSanitizerTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *logSanitizerTestPlugin) LogCapturePolicy() base.LogCapturePolicy {
	return base.LogCapturePolicy{RequestBodyBytes: 6}
}

func (p *logSanitizerTestPlugin) SanitizeLogSnapshot(snapshot *base.LogSnapshot) error {
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	*p.order = append(*p.order, p.GetName()+":sanitize")
	snapshot.Request.Header.Set("Authorization", "[REDACTED]")
	snapshot.Request.Body = []byte("masked")
	return p.err
}

func (p *logSanitizerSelectorTestPlugin) ShouldSanitizeLogSnapshot(snapshot base.LogSnapshot) bool {
	if p.selectorPanic != nil {
		panic(p.selectorPanic)
	}
	if p.selectorSeen != nil {
		*p.selectorSeen = append(*p.selectorSeen, string(snapshot.Request.Body))
	}
	if p.selectSnapshot != nil {
		return p.selectSnapshot(snapshot)
	}
	return true
}

func (*logExecutorNoCallbackPlugin) Init() error                            { return nil }
func (*logExecutorNoCallbackPlugin) PostInit() error                        { return nil }
func (p *logExecutorNoCallbackPlugin) Config() any                          { return p.config }
func (*logExecutorNoCallbackPlugin) Handler(next http.Handler) http.Handler { return next }

type failingBindingPhaseConfig struct{}

func (failingBindingPhaseConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return base.BindingPhaseDescriptor{}, errors.New("phase failed")
}

type nonComparableReadCloser []byte

func (nonComparableReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (nonComparableReadCloser) Close() error             { return nil }

func (b *failingLogBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, errors.New("request body failed")
	}
	b.read = true
	return copy(p, "ab"), errors.New("request body failed")
}

func (*failingLogBody) Close() error { return nil }

func (b *countingLogBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.bytesRead += n
	return n, err
}

func (*countingLogBody) Close() error { return nil }

func (p *logExecutorTestPlugin) Init() error                            { return nil }
func (p *logExecutorTestPlugin) PostInit() error                        { return nil }
func (p *logExecutorTestPlugin) Config() any                            { return nil }
func (p *logExecutorTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *logExecutorTestPlugin) RunLogPhase(snapshot base.LogSnapshot) error {
	p.mu.Lock()
	p.seen = append(p.seen, snapshot)
	if p.order != nil {
		*p.order = append(*p.order, p.GetName()+":log")
	}
	p.mu.Unlock()
	if p.panicLog {
		panic("log callback")
	}
	if p.panicLogValue != nil {
		panic(p.panicLogValue)
	}
	return nil
}

func (p *logExecutorTestPlugin) RunSnapshotFinalizer(base.LogSnapshot) error {
	p.mu.Lock()
	if p.order != nil {
		*p.order = append(*p.order, p.GetName()+":finalizer")
	}
	p.mu.Unlock()
	if p.panicFinalizer != nil {
		panic(p.panicFinalizer)
	}
	if p.finalizer {
		return errors.New("finalizer callback")
	}
	return nil
}

func newLogExecutorTestPlugin(name string, priority int, order *[]string) *logExecutorTestPlugin {
	plugin := &logExecutorTestPlugin{order: order}
	plugin.Name = name
	plugin.SetPriority(priority)
	return plugin
}

func TestLogExecutorFreezesLegacyIdentityAndPolicyAtConstruction(t *testing.T) {
	plugin := &logIdentityPolicyPlugin{
		name:   "legacy-logger",
		policy: base.LogCapturePolicy{RequestBodyBytes: 17, ResponseBodyBytes: 23},
	}
	executor, err := NewLogExecutor([]LogBinding{{Plugin: plugin}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	bindings := executor.Bindings()
	if len(bindings) != 1 || bindings[0].Factory != "legacy-logger" ||
		bindings[0].Policy != plugin.policy {
		t.Fatalf("frozen legacy binding = %#v", bindings)
	}
	if plugin.nameCalls != 1 || plugin.policyCalls != 1 {
		t.Fatalf("constructor name/policy calls = %d/%d, want 1/1", plugin.nameCalls, plugin.policyCalls)
	}
	if result := finalizeLogExecutorForTest(t, executor); len(result.Failures) != 0 {
		t.Fatalf("legacy finalization failures = %#v", result.Failures)
	}
	if plugin.nameCalls != 1 || plugin.policyCalls != 1 {
		t.Fatalf("request-time name/policy calls = %d/%d, want 1/1", plugin.nameCalls, plugin.policyCalls)
	}
}

func TestLogExecutorFromBindingsConsumesFrozenFactoryAndPolicy(t *testing.T) {
	plugin := &logIdentityPolicyPlugin{
		name:   "mutable-runtime-name",
		policy: base.LogCapturePolicy{RequestBodyBytes: 17, ResponseBodyBytes: 23},
	}
	binding, err := BindPluginChecked(
		"http-logger",
		plugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked() error = %v", err)
	}
	nameCalls, policyCalls := plugin.nameCalls, plugin.policyCalls
	plugin.name = "changed-after-binding"
	plugin.policy = base.LogCapturePolicy{RequestBodyBytes: 99, ResponseBodyBytes: 101}
	executor, err := NewLogExecutorFromBindings([]Binding{binding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	bindings := executor.Bindings()
	if len(bindings) != 1 || bindings[0].Factory != "http-logger" ||
		bindings[0].Policy != (base.LogCapturePolicy{RequestBodyBytes: 17, ResponseBodyBytes: 23}) {
		t.Fatalf("materialized log binding = %#v", bindings)
	}
	if plugin.nameCalls != nameCalls || plugin.policyCalls != policyCalls {
		t.Fatalf(
			"log construction re-read name/policy: before=%d/%d after=%d/%d",
			nameCalls,
			policyCalls,
			plugin.nameCalls,
			plugin.policyCalls,
		)
	}
	if result := finalizeLogExecutorForTest(t, executor); len(result.Failures) != 0 {
		t.Fatalf("materialized finalization failures = %#v", result.Failures)
	}
	if plugin.nameCalls != nameCalls || plugin.policyCalls != policyCalls {
		t.Fatalf(
			"request-time name/policy calls changed: before=%d/%d after=%d/%d",
			nameCalls,
			policyCalls,
			plugin.nameCalls,
			plugin.policyCalls,
		)
	}
}

func finalizeLogExecutorForTest(t *testing.T, executor LogExecutor) ctx.FinalizationResult {
	t.Helper()
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	request, err := executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !executor.RegisterComposite(request) {
		t.Fatal("RegisterComposite() = false")
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Unix(2, 0),
	)
	return lifecycle.FinalizeResult()
}

func requireLogPanicError(
	t *testing.T,
	result ctx.FinalizationResult,
	factory string,
	phase Phase,
	want any,
) {
	t.Helper()
	if len(result.Failures) != 1 {
		t.Fatalf("finalization failures = %#v, want one", result.Failures)
	}
	panicErr, ok := result.Failures[0].Err.(*PanicError)
	if !ok || panicErr.Factory != factory || panicErr.Phase != phase ||
		panicErr.Value != want || len(panicErr.Stack) == 0 {
		t.Fatalf("finalization failure = %#v, want attributed panic", result.Failures[0])
	}
}

func TestLogCallbackPanicUsesCanonicalFactoryAndContinues(t *testing.T) {
	want := errors.New("sensitive log panic")
	order := []string{}
	panicking := newLogExecutorTestPlugin("mutable-runtime-name", 20, &order)
	panicking.panicLogValue = want
	later := newLogExecutorTestPlugin("later", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Factory: "http-logger", Plugin: panicking, Scope: ScopeRoute},
		{Factory: "file-logger", Plugin: later, Scope: ScopeRoute},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	result := finalizeLogExecutorForTest(t, executor)
	requireLogPanicError(t, result, "http-logger", PhaseLog, want)
	if len(later.seen) != 1 || !reflect.DeepEqual(order, []string{
		"mutable-runtime-name:log",
		"later:log",
		"mutable-runtime-name:finalizer",
		"later:finalizer",
	}) {
		t.Fatalf("continued log order/seen = %v/%d", order, len(later.seen))
	}
}

func TestSanitizerSelectorPanicFailsClosed(t *testing.T) {
	want := errors.New("sensitive selector panic")
	order := []string{}
	sanitizer := &logSanitizerSelectorTestPlugin{
		logSanitizerTestPlugin: logSanitizerTestPlugin{order: &order},
		selectorPanic:          want,
	}
	sanitizer.Name = "mutable-sanitizer-name"
	later := newLogExecutorTestPlugin("later", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Factory: "key-auth", Plugin: sanitizer, Scope: ScopeRoute},
		{Factory: "http-logger", Plugin: later, Scope: ScopeRoute},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	result := finalizeLogExecutorForTest(t, executor)
	requireLogPanicError(t, result, "key-auth", PhaseLog, want)
	if len(later.seen) != 0 || len(order) != 0 {
		t.Fatalf("selector panic exposed snapshot to callbacks: order=%v seen=%d", order, len(later.seen))
	}
}

func TestSanitizerPanicFailsClosed(t *testing.T) {
	want := errors.New("sensitive sanitizer panic")
	order := []string{}
	sanitizer := &logSanitizerTestPlugin{order: &order, panicValue: want}
	sanitizer.Name = "mutable-sanitizer-name"
	later := newLogExecutorTestPlugin("later", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Factory: "key-auth", Plugin: sanitizer, Scope: ScopeRoute},
		{Factory: "http-logger", Plugin: later, Scope: ScopeRoute},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	result := finalizeLogExecutorForTest(t, executor)
	requireLogPanicError(t, result, "key-auth", PhaseLog, want)
	if len(later.seen) != 0 || len(order) != 0 {
		t.Fatalf("sanitizer panic exposed snapshot to callbacks: order=%v seen=%d", order, len(later.seen))
	}
}

func TestSnapshotFinalizerPanicUsesCanonicalFactoryAndContinues(t *testing.T) {
	want := errors.New("sensitive snapshot finalizer panic")
	order := []string{}
	panicking := newLogExecutorTestPlugin("mutable-runtime-name", 20, &order)
	panicking.panicFinalizer = want
	later := newLogExecutorTestPlugin("later", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Factory: "http-logger", Plugin: panicking, Scope: ScopeRoute},
		{Factory: "file-logger", Plugin: later, Scope: ScopeRoute},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	result := finalizeLogExecutorForTest(t, executor)
	requireLogPanicError(t, result, "http-logger", PhaseFinalizer, want)
	if !reflect.DeepEqual(order, []string{
		"mutable-runtime-name:log",
		"later:log",
		"mutable-runtime-name:finalizer",
		"later:finalizer",
	}) {
		t.Fatalf("snapshot finalizer order = %v, want continuation", order)
	}
}

func TestLogCompositeDistinguishesCorePanicFromGuardedCallbackFailure(t *testing.T) {
	t.Run("guarded callback remains bounded", func(t *testing.T) {
		want := errors.New("bounded callback panic")
		plugin := newLogExecutorTestPlugin("mutable-runtime-name", 10, nil)
		plugin.panicLogValue = want
		executor, err := NewLogExecutor([]LogBinding{{
			Factory: "http-logger",
			Plugin:  plugin,
			Scope:   ScopeRoute,
		}})
		if err != nil {
			t.Fatalf("NewLogExecutor() error = %v", err)
		}
		result := finalizeLogExecutorForTest(t, executor)
		requireLogPanicError(t, result, "http-logger", PhaseLog, want)
		if result.Failures[0].Kind != ctx.FinalizerOwnerCoreInvariant || result.FatalPanic != nil {
			t.Fatalf("guarded callback finalization = %#v, want bounded core-owned error", result)
		}
	})

	t.Run("raw orchestration panic is fatal", func(t *testing.T) {
		plugin := newLogExecutorTestPlugin("logger", 10, nil)
		executor, err := NewLogExecutor([]LogBinding{{
			Factory: "http-logger",
			Plugin:  plugin,
			Scope:   ScopeRoute,
		}})
		if err != nil {
			t.Fatalf("NewLogExecutor() error = %v", err)
		}
		request, lifecycle := ctx.EnsureRequestLifecycle(
			httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
			time.Unix(1, 0),
		)
		request, err = executor.Prepare(request)
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		if !executor.RegisterComposite(request) {
			t.Fatal("RegisterComposite() = false")
		}
		request.URL = nil
		lifecycle.Complete(
			ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
			time.Unix(2, 0),
		)
		result := lifecycle.FinalizeResult()
		if len(result.Failures) != 1 || result.Failures[0].Kind != ctx.FinalizerOwnerCoreInvariant ||
			result.Failures[0].PanicValue == nil || result.FatalPanic == nil {
			t.Fatalf("raw orchestration finalization = %#v, want fatal core panic", result)
		}
	})
}

func TestLogSnapshotSanitizerRunsBeforeLoggerAndFinalizer(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("secret-body")),
		time.Unix(1, 0),
	)
	request.Header.Set("Authorization", "Bearer secret")
	order := []string{}
	sanitizer := &logSanitizerTestPlugin{order: &order}
	sanitizer.Name = "sanitizer"
	sanitizer.SetPriority(100)
	loggerPlugin := newLogExecutorTestPlugin("logger", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Plugin: loggerPlugin, Scope: ScopeRoute, Policy: base.LogCapturePolicy{RequestBodyBytes: 11}},
		{Plugin: sanitizer, Scope: ScopeGlobal, Policy: sanitizer.LogCapturePolicy()},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if got, want := order, []string{
		"sanitizer:sanitize",
		"logger:log",
		"logger:finalizer",
	}; !reflect.DeepEqual(
		got,
		want,
	) {
		t.Fatalf("callback order = %v, want %v", got, want)
	}
	if got := loggerPlugin.seen[0].Request.Header.Get("Authorization"); got != "[REDACTED]" {
		t.Fatalf("logger Authorization = %q, want redacted", got)
	}
	if got := string(loggerPlugin.seen[0].Request.Body); got != "masked" {
		t.Fatalf("logger body = %q, want masked", got)
	}
	loggerPlugin.seen[0].Request.Header.Set("Authorization", "mutated")
	if got := request.Header.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("live request Authorization = %q, want unchanged", got)
	}
}

func TestLogSnapshotSanitizerErrorStopsRawSnapshotConsumers(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("secret-body")),
		time.Unix(1, 0),
	)
	order := []string{}
	sanitizer := &logSanitizerTestPlugin{order: &order, err: errors.New("sanitize failed")}
	sanitizer.Name = "sanitizer"
	loggerPlugin := newLogExecutorTestPlugin("logger", 10, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Plugin: sanitizer, Scope: ScopeRoute, Policy: sanitizer.LogCapturePolicy()},
		{Plugin: loggerPlugin, Scope: ScopeRoute, Policy: base.LogCapturePolicy{RequestBodyBytes: 11}},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Unix(2, 0),
	)
	failures := lifecycle.Finalize()
	if len(failures) != 1 || !strings.Contains(failures[0].Err.Error(), "sanitize failed") {
		t.Fatalf("Finalize() failures = %#v, want sanitizer failure", failures)
	}
	if got, want := order, []string{"sanitizer:sanitize"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("callback order = %v, want %v", got, want)
	}
	if len(loggerPlugin.seen) != 0 {
		t.Fatalf("logger saw raw snapshot after sanitizer error: %#v", loggerPlugin.seen)
	}
}

func TestLogSnapshotSanitizerSelectorsSharePreSanitizedState(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("secret-body")),
		time.Unix(1, 0),
	)
	seen := []string{}
	order := []string{}
	first := &logSanitizerSelectorTestPlugin{
		logSanitizerTestPlugin: logSanitizerTestPlugin{order: &order},
		selectorSeen:           &seen,
	}
	first.Name = "first"
	first.SetPriority(20)
	first.selectSnapshot = func(snapshot base.LogSnapshot) bool {
		return true
	}
	second := &logSanitizerSelectorTestPlugin{
		logSanitizerTestPlugin: logSanitizerTestPlugin{order: &order},
		selectorSeen:           &seen,
	}
	second.Name = "second"
	second.SetPriority(10)
	second.selectSnapshot = func(snapshot base.LogSnapshot) bool {
		return string(snapshot.Request.Body) == "secret-body"
	}
	logger := newLogExecutorTestPlugin("logger", 1, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Plugin: first, Scope: ScopeRoute, Policy: first.LogCapturePolicy()},
		{Plugin: second, Scope: ScopeRoute, Policy: second.LogCapturePolicy()},
		{Plugin: logger, Scope: ScopeRoute, Policy: base.LogCapturePolicy{RequestBodyBytes: 11}},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if !reflect.DeepEqual(seen, []string{"secret-body", "secret-body"}) {
		t.Fatalf("selector snapshots = %q, want two original snapshots", seen)
	}
	if len(logger.seen) != 1 || string(logger.seen[0].Request.Body) != "masked" {
		t.Fatalf("logger snapshot after selected sanitizers = %#v", logger.seen)
	}
}

func TestLogExecutorSealAndRegisterAreIdempotent(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/", strings.NewReader("request-body"))
	request, lifecycle := ctx.EnsureRequestLifecycle(request, time.Unix(1, 0))
	wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
	_ = wrapped
	request = base.WithResponseCapture(request, capture)
	order := []string{}
	global := newLogExecutorTestPlugin("global", 20, &order)
	route := newLogExecutorTestPlugin("route", 30, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Plugin: route, Scope: ScopeRoute, Policy: base.LogCapturePolicy{RequestBodyBytes: 4}},
		{Plugin: global, Scope: ScopeGlobal},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealFinalRequest(request); err != nil {
		t.Fatalf("SealFinalRequest() error = %v", err)
	}
	if err := executor.SealFinalRequest(request); err != nil {
		t.Fatalf("second SealFinalRequest() error = %v", err)
	}
	if !executor.RegisterComposite(request) {
		t.Fatal("first RegisterComposite() = false")
	}
	if executor.RegisterComposite(request) {
		t.Fatal("second RegisterComposite() = true")
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if got, want := order, []string{
		"global:log", "route:log", "global:finalizer", "route:finalizer",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("callback order = %v, want %v", got, want)
	}
	if got := string(global.seen[0].Request.Body); got != "" {
		t.Fatalf("global zero policy body = %q, want empty", got)
	}
	if got := string(route.seen[0].Request.Body); got != "requ" {
		t.Fatalf("route bounded body = %q, want requ", got)
	}
}

func TestLogExecutorPrepareRestoresFullRequestBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("abcdefgh"))
	request, _ = ctx.EnsureRequestLifecycle(request, time.Now())
	executor, err := NewLogExecutor([]LogBinding{{
		Plugin: newLogExecutorTestPlugin("logger", 1, nil),
		Policy: base.LogCapturePolicy{RequestBodyBytes: 4},
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealFinalRequest(request); err != nil {
		t.Fatalf("SealFinalRequest() error = %v", err)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(body) != "abcdefgh" {
		t.Fatalf("restored body = %q, want abcdefgh", body)
	}
}

func TestLogExecutorCallbackPanicDoesNotSkipLaterCallback(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil), time.Now(),
	)
	order := []string{}
	panicPlugin := newLogExecutorTestPlugin("panic", 200, &order)
	panicPlugin.panicLog = true
	later := newLogExecutorTestPlugin("later", 100, &order)
	executor, err := NewLogExecutor([]LogBinding{
		{Plugin: panicPlugin, Scope: ScopeRoute}, {Plugin: later, Scope: ScopeRoute},
	})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if !executor.RegisterComposite(request) {
		t.Fatal("RegisterComposite() = false")
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 1 {
		t.Fatalf("Finalize() failures = %#v, want callback panic/error", failures)
	}
	if !reflect.DeepEqual(order, []string{"panic:log", "later:log", "panic:finalizer", "later:finalizer"}) {
		t.Fatalf("callback order = %v", order)
	}
}

func TestLogExecutorFinalSnapshotUsesPreparedBodyAfterTerminalConsumption(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request-body")), time.Now(),
	)
	logger := newLogExecutorTestPlugin("logger", 1, nil)
	executor, err := NewLogExecutor([]LogBinding{{
		Plugin: logger,
		Scope:  ScopeRoute,
		Policy: base.LogCapturePolicy{RequestBodyBytes: 4},
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := io.ReadAll(request.Body); err != nil {
		t.Fatalf("terminal body read: %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if got := string(logger.seen[0].Request.Body); got != "requ" {
		t.Fatalf("captured request body = %q, want %q", got, "requ")
	}
	if !logger.seen[0].Request.BodyTruncated {
		t.Fatal("captured request body truncated = false, want true")
	}
}

func TestLogExecutorSealRecapturesSameRequestAfterBodyRewrite(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("original-body")), time.Now(),
	)
	logger := newLogExecutorTestPlugin("logger", 1, nil)
	executor, err := NewLogExecutor([]LogBinding{{
		Plugin: logger,
		Scope:  ScopeRoute,
		Policy: base.LogCapturePolicy{RequestBodyBytes: 16},
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	base.ReplaceRequestBody(request, []byte("rewritten-body"))
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Now(),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if got := string(logger.seen[0].Request.Body); got != "rewritten-body" {
		t.Fatalf("captured body = %q, want rewritten-body", got)
	}
}

func TestLogExecutorMaterializedBindingsIncreasePreparedCapture(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request-body")), time.Now(),
	)
	staticExecutor, err := NewLogExecutor([]LogBinding{{
		Plugin: newLogExecutorTestPlugin("static", 1, nil),
		Scope:  ScopeGlobal,
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor(static) error = %v", err)
	}
	request, err = staticExecutor.Prepare(request)
	if err != nil {
		t.Fatalf("static Prepare() error = %v", err)
	}
	consumer := newLogExecutorTestPlugin("consumer", 1, nil)
	materialized, err := staticExecutor.WithBindings([]LogBinding{{
		Plugin: consumer,
		Scope:  ScopeRoute,
		Policy: base.LogCapturePolicy{RequestBodyBytes: 4},
	}})
	if err != nil {
		t.Fatalf("WithBindings() error = %v", err)
	}
	request, err = materialized.Prepare(request)
	if err != nil {
		t.Fatalf("materialized Prepare() error = %v", err)
	}
	if _, err := io.ReadAll(request.Body); err != nil {
		t.Fatalf("terminal body read: %v", err)
	}
	if err := materialized.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Now())
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if got := string(consumer.seen[0].Request.Body); got != "requ" {
		t.Fatalf("materialized captured request body = %q, want %q", got, "requ")
	}
	if !consumer.seen[0].Request.BodyTruncated {
		t.Fatal("materialized captured request body truncated = false, want true")
	}
}

func TestLogExecutorFinalSnapshotDoesNotReadLiveBodyAgain(t *testing.T) {
	body := &countingLogBody{reader: strings.NewReader("abcdefgh")}
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/", nil)
	request.Body = body
	request.ContentLength = 8
	request, lifecycle := ctx.EnsureRequestLifecycle(request, time.Unix(1, 0))

	loggerPlugin := newLogExecutorTestPlugin("logger", 1, nil)
	executor, err := NewLogExecutor([]LogBinding{{
		Plugin: loggerPlugin,
		Scope:  ScopeRoute,
		Policy: base.LogCapturePolicy{RequestBodyBytes: 4},
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	readAfterPrepare := body.bytesRead
	if readAfterPrepare == 0 {
		t.Fatal("Prepare() did not capture the bounded body prefix")
	}
	if !executor.RegisterComposite(request) {
		t.Fatal("RegisterComposite() = false")
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if body.bytesRead != readAfterPrepare {
		t.Fatalf(
			"final snapshot read live body again: bytes = %d, want %d",
			body.bytesRead,
			readAfterPrepare,
		)
	}
	if got := string(loggerPlugin.seen[0].Request.Body); got != "abcd" {
		t.Fatalf("captured request body = %q, want %q", got, "abcd")
	}
}

func TestLogExecutorOriginalRegistrationUsesMaterializedBindings(t *testing.T) {
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	staticPlugin := newLogExecutorTestPlugin("static", 1, nil)
	materializedPlugin := newLogExecutorTestPlugin("materialized", 2, nil)
	staticExecutor, err := NewLogExecutor([]LogBinding{{Plugin: staticPlugin, Scope: ScopeRoute}})
	if err != nil {
		t.Fatalf("NewLogExecutor(static) error = %v", err)
	}
	request, err = staticExecutor.Prepare(request)
	if err != nil {
		t.Fatalf("static Prepare() error = %v", err)
	}
	materializedExecutor, err := staticExecutor.WithBindings([]LogBinding{{
		Plugin: materializedPlugin,
		Scope:  ScopeConsumer,
	}})
	if err != nil {
		t.Fatalf("WithBindings() error = %v", err)
	}
	request, err = materializedExecutor.Prepare(request)
	if err != nil {
		t.Fatalf("materialized Prepare() error = %v", err)
	}
	if !staticExecutor.RegisterComposite(request) {
		t.Fatal("static RegisterComposite() = false")
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(staticPlugin.seen) != 0 {
		t.Fatalf("static callback count = %d, want 0 after materialization", len(staticPlugin.seen))
	}
	if len(materializedPlugin.seen) != 1 {
		t.Fatalf("materialized callback count = %d, want 1", len(materializedPlugin.seen))
	}
}

func TestRequestPipelineRegistersLogCompositeBeforeTerminalPanic(t *testing.T) {
	loggerPlugin := newLogExecutorTestPlugin("logger", 1, nil)
	binding := BindPlugin(
		"http-logger",
		loggerPlugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
	)
	logExecutor, err := NewLogExecutorFromBindings([]Binding{binding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	handler := NewRequestPipeline([]Binding{binding}, nil).
		WithLogExecutor(&logExecutor).
		Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("request stage failure")
		}))

	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
	request = base.WithResponseCapture(request, capture)
	func() {
		defer func() {
			if recovered := recover(); recovered != "request stage failure" {
				t.Fatalf("recovered = %#v", recovered)
			}
		}()
		handler.ServeHTTP(wrapped, request)
	}()
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeRecoveredPanic, Status: http.StatusInternalServerError},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(loggerPlugin.seen) != 1 {
		t.Fatalf("log callback count = %d, want 1", len(loggerPlugin.seen))
	}
}

func TestRequestPipelineUsesResolvedConsumerLogWinner(t *testing.T) {
	routeLogger := newLogExecutorTestPlugin("route", 1, nil)
	consumerLogger := newLogExecutorTestPlugin("consumer", 2, nil)
	routeBinding := BindPlugin(
		"http-logger",
		routeLogger,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
	)
	consumerBinding := BindPlugin(
		"http-logger",
		consumerLogger,
		ScopeConsumer,
		ResourceProvenance{Kind: ResourceConsumer, ID: "consumer-1"},
	)
	logExecutor, err := NewLogExecutorFromBindings([]Binding{routeBinding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	pipeline := NewRequestPipeline([]Binding{routeBinding}, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Bindings: []Binding{consumerBinding}, Resolved: true}, nil
	}).WithLogExecutor(&logExecutor)

	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
	request = base.WithResponseCapture(request, capture)
	pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(wrapped, request)
	lifecycle.Complete(capture.Outcome(), time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(routeLogger.seen) != 0 {
		t.Fatalf("route logger count = %d, want 0 after consumer override", len(routeLogger.seen))
	}
	if len(consumerLogger.seen) != 1 {
		t.Fatalf("consumer logger count = %d, want 1", len(consumerLogger.seen))
	}
}

func TestPrometheusLogBindingPrefersRouteOverGlobal(t *testing.T) {
	global := newLogExecutorTestPlugin("global-prometheus", 1, nil)
	route := newLogExecutorTestPlugin("route-prometheus", 2, nil)
	bindings := []Binding{
		BindPlugin("prometheus", global, ScopeGlobal, ResourceProvenance{
			Kind: ResourceGlobalRule,
			ID:   "global-1",
		}),
		BindPlugin("prometheus", route, ScopeRoute, ResourceProvenance{
			Kind: ResourceRoute,
			ID:   "route-1",
		}),
	}
	executor, err := NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(global.seen) != 0 {
		t.Fatalf("global prometheus callback count = %d, want 0", len(global.seen))
	}
	if len(route.seen) != 1 {
		t.Fatalf("route prometheus callback count = %d, want 1", len(route.seen))
	}
}

func TestPrometheusLogBindingUsesGlobalWhenRouteAbsent(t *testing.T) {
	global := newLogExecutorTestPlugin("global-prometheus", 1, nil)
	binding := BindPlugin("prometheus", global, ScopeGlobal, ResourceProvenance{
		Kind: ResourceGlobalRule,
		ID:   "global-1",
	})
	executor, err := NewLogExecutorFromBindings([]Binding{binding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	request, err = executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := executor.SealAndRegister(request); err != nil {
		t.Fatalf("SealAndRegister() error = %v", err)
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(global.seen) != 1 {
		t.Fatalf("global prometheus callback count = %d, want 1", len(global.seen))
	}
}

func TestPrometheusConsumerBindingRunsWithEmptyStaticLogExecutor(t *testing.T) {
	consumer := newLogExecutorTestPlugin("consumer-prometheus", 2, nil)
	consumerBinding := BindPlugin("prometheus", consumer, ScopeConsumer, ResourceProvenance{
		Kind: ResourceConsumer,
		ID:   "consumer-1",
	})
	emptyExecutor, err := NewLogExecutorFromBindings(nil)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Bindings: []Binding{consumerBinding}, Resolved: true}, nil
	}).WithLogExecutor(&emptyExecutor)
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusNoContent},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(consumer.seen) != 1 {
		t.Fatalf("consumer prometheus callback count = %d, want 1", len(consumer.seen))
	}
}

func TestEmptyLogExecutorSkipsRequestStateAndFinalizer(t *testing.T) {
	requestContext := &requestcontext.Plugin{}
	binding := BindPlugin("request-context", requestContext, ScopeSystem, ResourceProvenance{
		Kind: ResourceSystem,
		ID:   "request-context",
	})
	executor, err := NewLogExecutorFromBindings([]Binding{binding})
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}
	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	prepared, err := executor.Prepare(request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared != request {
		t.Fatal("empty log executor replaced request")
	}
	if logStateFromRequest(request) != nil {
		t.Fatal("empty log executor allocated request log state")
	}
	if executor.RegisterComposite(request) {
		t.Fatal("empty log executor registered a lifecycle finalizer")
	}
	lifecycle.Complete(ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusOK}, time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
}

func TestRequestPipelineLogsFinalReplacementRequest(t *testing.T) {
	loggerPlugin := newLogExecutorTestPlugin("logger", 1, nil)
	auth := newExecutorRequestPlugin(
		"jwt-auth",
		10,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			replacement := r.Clone(r.Context())
			replacement.Method = http.MethodPost
			replacement.URL.Path = "/replacement"
			return base.ContinueRequest(replacement)
		},
	)
	bindings := []Binding{
		BindPlugin(
			"jwt-auth",
			auth,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
		),
		BindPlugin(
			"http-logger",
			loggerPlugin,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
		),
	}
	logExecutor, err := NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}

	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/original", nil),
		time.Unix(1, 0),
	)
	wrapped, capture := base.CaptureResponseOutcomeController(httptest.NewRecorder())
	request = base.WithResponseCapture(request, capture)
	NewRequestPipeline(bindings, nil).
		WithLogExecutor(&logExecutor).
		Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).
		ServeHTTP(wrapped, request)

	lifecycle.Complete(capture.Outcome(), time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(loggerPlugin.seen) != 1 {
		t.Fatalf("log callback count = %d, want 1", len(loggerPlugin.seen))
	}
	if got := loggerPlugin.seen[0].Request.Method; got != http.MethodPost {
		t.Fatalf("snapshot method = %q, want POST", got)
	}
	if got := loggerPlugin.seen[0].Request.URI; got != "/replacement" {
		t.Fatalf("snapshot URI = %q, want /replacement", got)
	}
}

func TestRequestPipelineAuthStopWithBufferedResponseRegistersLogComposite(t *testing.T) {
	loggerPlugin := newLogExecutorTestPlugin("logger", 1, nil)
	auth := newExecutorRequestPlugin(
		"key-auth",
		10,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.WriteHeader(http.StatusUnauthorized)
			return base.StopRequestWithSource(r, ctx.ResponseSourceAPISIX)
		},
	)
	bounded := newResponseTestPlugin(
		"response-rewrite",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	bindings := []Binding{
		pipelineBinding("basic-auth", auth, ScopeRoute, 10),
		checkedResponseBinding(t, "response-rewrite", bounded, ScopeRoute, "route-1"),
		BindPlugin(
			"http-logger",
			loggerPlugin,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
		),
	}
	logExecutor, err := NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}

	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Unix(1, 0),
	)
	response := httptest.NewRecorder()
	NewRequestPipeline(bindings, nil).
		WithBufferedResponseExecutor(newBufferedTestExecutor(t, bindings)).
		WithLogExecutor(&logExecutor).
		Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("terminal called after authentication stop")
		})).
		ServeHTTP(response, request)

	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusUnauthorized},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(loggerPlugin.seen) != 1 {
		t.Fatalf("log callback count = %d, want 1", len(loggerPlugin.seen))
	}
}

func TestRequestPipelineAuthStopStillSanitizesKeyAuthLogSnapshot(t *testing.T) {
	keyAuth := &keyauth.Plugin{}
	if err := keyAuth.Init(); err != nil {
		t.Fatalf("key-auth Init() error = %v", err)
	}
	if err := keyAuth.PostInit(); err != nil {
		t.Fatalf("key-auth PostInit() error = %v", err)
	}
	stopper := newExecutorRequestPlugin(
		"higher-priority-stop",
		3000,
		func(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			w.WriteHeader(http.StatusForbidden)
			return base.StopRequestWithSource(r, ctx.ResponseSourceAPISIX)
		},
	)
	logger := newLogExecutorTestPlugin("logger", 1, nil)
	bindings := []Binding{
		pipelineBinding("basic-auth", stopper, ScopeRoute, 3000),
		pipelineBinding("key-auth", keyAuth, ScopeRoute, 2500),
		pipelineBinding("http-logger", logger, ScopeRoute, 1),
	}
	logExecutor, err := NewLogExecutorFromBindings(bindings)
	if err != nil {
		t.Fatalf("NewLogExecutorFromBindings() error = %v", err)
	}

	request, lifecycle := ctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/orders?apikey=secret&keep=yes", nil),
		time.Unix(1, 0),
	)
	request.Header.Set("apikey", "secret")
	response := httptest.NewRecorder()
	NewRequestPipeline(bindings, nil).
		WithLogExecutor(&logExecutor).
		Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("terminal called after higher-priority stop")
		})).
		ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want %d", response.Code, http.StatusForbidden)
	}
	lifecycle.Complete(
		ctx.ResponseOutcome{Kind: ctx.RequestOutcomeCompleted, Status: http.StatusForbidden},
		time.Unix(2, 0),
	)
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(logger.seen) != 1 {
		t.Fatalf("logger snapshot count = %d, want 1", len(logger.seen))
	}
	snapshot := logger.seen[0]
	if got := snapshot.Request.Header.Get("apikey"); got != "" {
		t.Fatalf("sanitized apikey header = %q, want removed", got)
	}
	if snapshot.Request.URI != "/orders?keep=yes" {
		t.Fatalf("sanitized request URI = %q, want apikey removed", snapshot.Request.URI)
	}
}

func TestRequestPipelinePreparationErrorRegistersLogComposite(t *testing.T) {
	loggerPlugin := newLogExecutorTestPlugin("logger", 1, nil)
	binding := BindPlugin(
		"http-logger",
		loggerPlugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
	)
	logExecutor, err := NewLogExecutor([]LogBinding{{
		Plugin: loggerPlugin,
		Scope:  ScopeRoute,
		Policy: base.LogCapturePolicy{RequestBodyBytes: 4},
	}})
	if err != nil {
		t.Fatalf("NewLogExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://gateway.test/", nil)
	request.Body = &failingLogBody{}
	request.ContentLength = 4
	request, lifecycle := ctx.EnsureRequestLifecycle(request, time.Unix(1, 0))
	response := httptest.NewRecorder()
	w, capture := base.CaptureResponseOutcomeController(response)
	request = base.WithResponseCapture(request, capture)

	NewRequestPipeline([]Binding{binding}, nil).
		WithLogExecutor(&logExecutor).
		Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("terminal called after log preparation failure")
		})).
		ServeHTTP(w, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want 500", response.Code)
	}
	lifecycle.Complete(capture.Outcome(), time.Unix(2, 0))
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("Finalize() failures = %#v", failures)
	}
	if len(loggerPlugin.seen) != 1 {
		t.Fatalf("log callback count = %d, want 1", len(loggerPlugin.seen))
	}
	if got := loggerPlugin.seen[0].Outcome.Status; got != http.StatusInternalServerError {
		t.Fatalf("snapshot response status = %d, want 500", got)
	}
	if got := loggerPlugin.seen[0].Source; got != ctx.ResponseSourceAPISIX {
		t.Fatalf("snapshot source = %q, want apisix", got)
	}
	if got := loggerPlugin.seen[0].Request.Body; len(got) != 0 {
		t.Fatalf("snapshot request body = %q, want omitted after capture failure", got)
	}
}

func TestLogExecutorRejectsInvalidMaterializationAndLifecycleInputs(t *testing.T) {
	valid := newLogExecutorTestPlugin("logger", 1, nil)
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{
			name: "nil direct binding",
			run: func() error {
				_, err := NewLogExecutor([]LogBinding{{}})
				return err
			},
		},
		{
			name: "invalid capture policy",
			run: func() error {
				_, err := NewLogExecutor([]LogBinding{{
					Plugin: valid,
					Policy: base.LogCapturePolicy{RequestBodyBytes: -1},
				}})
				return err
			},
		},
		{
			name: "nil materialized plugin",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{Descriptor: Descriptor{Factory: "http-logger"}}})
				return err
			},
		},
		{
			name: "unknown factory",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					Descriptor: Descriptor{Factory: "unknown"}, Plugin: valid,
				}})
				return err
			},
		},
		{
			name: "missing log callback",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					Descriptor: Descriptor{Factory: "http-logger"}, Plugin: &logExecutorNoCallbackPlugin{},
				}})
				return err
			},
		},
		{
			name: "missing prometheus log callback",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					Descriptor: Descriptor{Factory: "prometheus"}, Plugin: &logExecutorNoCallbackPlugin{},
				}})
				return err
			},
		},
		{
			name: "serverless phase descriptor",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					Descriptor: Descriptor{Factory: "serverless-pre-function"},
					Plugin:     &logExecutorNoCallbackPlugin{config: failingBindingPhaseConfig{}},
				}})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("invalid log executor materialization unexpectedly succeeded")
			}
		})
	}

	executor, err := NewLogExecutor(nil)
	if err != nil {
		t.Fatalf("NewLogExecutor(nil) error = %v", err)
	}
	if _, err := executor.Prepare(nil); err == nil {
		t.Fatal("Prepare(nil) unexpectedly succeeded")
	}
	if err := executor.SealFinalRequest(nil); err == nil {
		t.Fatal("SealFinalRequest(nil) unexpectedly succeeded")
	}
	if executor.RegisterComposite(nil) {
		t.Fatal("RegisterComposite(nil) unexpectedly succeeded")
	}
	plain := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	if executor.RegisterComposite(plain) {
		t.Fatal("RegisterComposite() accepted request without log state")
	}
	withState := plain.WithContext(context.WithValue(plain.Context(), logRequestStateKey{}, &LogRequestState{}))
	if executor.RegisterComposite(withState) {
		t.Fatal("RegisterComposite() accepted request without lifecycle")
	}
	if logStateFromRequest(nil) != nil {
		t.Fatal("logStateFromRequest(nil) returned state")
	}
	writeStableLogPreparationError(nil, errors.New("ignored"))
}

func TestLogExecutorReadCloserSafetyBoundaries(t *testing.T) {
	if !sameReadCloser(nil, nil) || sameReadCloser(nil, http.NoBody) {
		t.Fatal("sameReadCloser() nil semantics changed")
	}
	left := nonComparableReadCloser("left")
	right := nonComparableReadCloser("right")
	if sameReadCloser(left, right) {
		t.Fatal("sameReadCloser() accepted non-comparable readers")
	}
	if sameReadCloser(http.NoBody, left) {
		t.Fatal("sameReadCloser() accepted different reader types")
	}
}
