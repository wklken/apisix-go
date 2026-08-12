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
)

type logExecutorTestPlugin struct {
	base.BasePlugin
	mu        sync.Mutex
	order     *[]string
	panicLog  bool
	finalizer bool
	seen      []base.LogSnapshot
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
	return nil
}

func (p *logExecutorTestPlugin) RunSnapshotFinalizer(base.LogSnapshot) error {
	p.mu.Lock()
	if p.order != nil {
		*p.order = append(*p.order, p.GetName()+":finalizer")
	}
	p.mu.Unlock()
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
		responseTestConfig{stage: "none", header: true},
	)
	bindings := []Binding{
		pipelineBinding("key-auth", auth, ScopeRoute, 10),
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
				_, err := NewLogExecutorFromBindings([]Binding{{factoryName: "http-logger"}})
				return err
			},
		},
		{
			name: "unknown factory",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					factoryName: "unknown", Plugin: valid,
				}})
				return err
			},
		},
		{
			name: "missing log callback",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					factoryName: "http-logger", Plugin: &logExecutorNoCallbackPlugin{},
				}})
				return err
			},
		},
		{
			name: "missing snapshot finalizer",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					factoryName: "request-context", Plugin: &logExecutorNoCallbackPlugin{},
				}})
				return err
			},
		},
		{
			name: "serverless phase descriptor",
			run: func() error {
				_, err := NewLogExecutorFromBindings([]Binding{{
					factoryName: "serverless-pre-function",
					Plugin:      &logExecutorNoCallbackPlugin{config: failingBindingPhaseConfig{}},
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

func TestLogExecutorReadCloserAndCallbackSafetyBoundaries(t *testing.T) {
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
	wantErr := errors.New("callback failed")
	if err := runLogCallback(func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("runLogCallback() error = %v", err)
	}
	if err := runLogCallback(func() error { panic("callback panic") }); err == nil {
		t.Fatal("runLogCallback() did not convert panic")
	}
}
