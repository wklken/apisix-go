package plugin

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
	corsplugin "github.com/wklken/apisix-go/pkg/plugin/cors"
)

type plan16StreamingPlugin struct {
	base.BasePlugin
	closes   *int
	finishes *int
}

type task10StreamingCloser struct {
	name  string
	order *[]string
	err   error
	panic any
}

func (c task10StreamingCloser) Close() error {
	*c.order = append(*c.order, c.name)
	if c.panic != nil {
		panic(c.panic)
	}
	return c.err
}

type task10StreamingFinalizer struct {
	name    string
	order   *[]string
	err     error
	panic   any
	cause   *error
	started chan<- struct{}
	release <-chan struct{}
	calls   *atomic.Int32
}

type task10RetainedDownstreamCall struct {
	name  string
	order *[]string
	calls *atomic.Int32
}

func (c task10RetainedDownstreamCall) run(w http.ResponseWriter) {
	if c.calls != nil {
		c.calls.Add(1)
	}
	if c.order != nil {
		*c.order = append(*c.order, c.name)
	}
	_, _ = w.Write(nil)
}

type task10RetainedDownstreamCloser struct {
	http.ResponseWriter
	call task10RetainedDownstreamCall
}

func (w task10RetainedDownstreamCloser) Close() error {
	w.call.run(w.ResponseWriter)
	return nil
}

type task10RetainedDownstreamFinalizer struct {
	http.ResponseWriter
	call task10RetainedDownstreamCall
}

func (w task10RetainedDownstreamFinalizer) FinishStreamingResponse(error) error {
	w.call.run(w.ResponseWriter)
	return nil
}

func (f task10StreamingFinalizer) FinishStreamingResponse(cause error) error {
	if f.calls != nil {
		f.calls.Add(1)
	}
	if f.cause != nil {
		*f.cause = cause
	}
	if f.order != nil {
		*f.order = append(*f.order, f.name)
	}
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.release != nil {
		<-f.release
	}
	if f.panic != nil {
		panic(f.panic)
	}
	return f.err
}

func TestStreamingFinishPanicDoesNotSkipRemainingCleanup(t *testing.T) {
	panicValue := errors.New("finish panic")
	order := make([]string, 0, 3)
	finish := &streamingFinish{finalizers: []streamingFinalizerEntry{
		{factory: "first", phase: PhaseBodyFilter, finalizer: task10StreamingFinalizer{name: "first", order: &order}},
		{
			factory: "panic", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{name: "panic", order: &order, panic: panicValue},
		},
		{factory: "last", phase: PhaseBodyFilter, finalizer: task10StreamingFinalizer{name: "last", order: &order}},
	}}

	result := finish.finish(nil)
	if !reflect.DeepEqual(order, []string{"last", "panic", "first"}) {
		t.Fatalf("finish order = %v, want reverse order with cleanup after panic", order)
	}
	if result.Err != nil || len(result.Panics) != 1 {
		t.Fatalf("finish result = %#v, want one panic and no ordinary error", result)
	}
	got := result.Panics[0]
	if got.Factory != "panic" || got.Phase != PhaseBodyFilter || got.Value != panicValue || len(got.Stack) == 0 {
		t.Fatalf("finish panic = %#v, want attributed panic", got)
	}
}

func TestStreamingFinishAbortSentinelPanicDoesNotSkipRemainingCleanup(t *testing.T) {
	tests := []struct {
		name   string
		finish func(*[]string) *streamingFinish
	}{
		{
			name: "closer",
			finish: func(order *[]string) *streamingFinish {
				return &streamingFinish{closers: []streamingCloserEntry{
					{
						factory: "first",
						phase:   PhaseBodyFilter,
						closer:  task10StreamingCloser{name: "first", order: order},
					},
					{
						factory: "abort", phase: PhaseBodyFilter,
						closer: task10StreamingCloser{name: "abort", order: order, panic: http.ErrAbortHandler},
					},
					{
						factory: "last",
						phase:   PhaseBodyFilter,
						closer:  task10StreamingCloser{name: "last", order: order},
					},
				}}
			},
		},
		{
			name: "finalizer",
			finish: func(order *[]string) *streamingFinish {
				return &streamingFinish{finalizers: []streamingFinalizerEntry{
					{
						factory: "first", phase: PhaseBodyFilter,
						finalizer: task10StreamingFinalizer{name: "first", order: order},
					},
					{
						factory: "abort", phase: PhaseBodyFilter,
						finalizer: task10StreamingFinalizer{name: "abort", order: order, panic: http.ErrAbortHandler},
					},
					{
						factory: "last", phase: PhaseBodyFilter,
						finalizer: task10StreamingFinalizer{name: "last", order: order},
					},
				}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			order := make([]string, 0, 3)
			result := test.finish(&order).finish(nil)
			if !reflect.DeepEqual(order, []string{"last", "abort", "first"}) {
				t.Fatalf("finish order = %v, want reverse order with cleanup after abort sentinel", order)
			}
			if result.Err != nil || len(result.Panics) != 1 {
				t.Fatalf("finish result = %#v, want one panic and no ordinary error", result)
			}
			panicErr := result.Panics[0]
			if panicErr.Factory != "abort" || panicErr.Phase != PhaseBodyFilter ||
				panicErr.Value != http.ErrAbortHandler || len(panicErr.Stack) == 0 {
				t.Fatalf("abort panic = %#v, want attributed sentinel", panicErr)
			}
		})
	}
}

func TestStreamingFinishPreservesFirstOrdinaryError(t *testing.T) {
	want := errors.New("close failed")
	order := make([]string, 0, 2)
	finish := &streamingFinish{closers: []streamingCloserEntry{
		{
			factory: "registered-first", phase: PhaseBodyFilter,
			closer: task10StreamingCloser{name: "registered-first", order: &order, err: errors.New("later failure")},
		},
		{
			factory: "registered-last", phase: PhaseBodyFilter,
			closer: task10StreamingCloser{name: "registered-last", order: &order, err: want},
		},
	}}

	result := finish.finish(nil)
	if !errors.Is(result.Err, want) || len(result.Panics) != 0 {
		t.Fatalf("finish result = %#v, want first reverse-order error", result)
	}
	if !reflect.DeepEqual(order, []string{"registered-last", "registered-first"}) {
		t.Fatalf("closer order = %v, want reverse registration order", order)
	}
}

func TestStreamingFinishConcurrentCallersWaitForCachedDetachedResult(t *testing.T) {
	wantErr := errors.New("finish error")
	panicValue := errors.New("finish panic")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	finish := &streamingFinish{finalizers: []streamingFinalizerEntry{
		{
			factory: "ordinary", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{err: wantErr},
		},
		{
			factory: "blocking", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{
				panic: panicValue, started: started, release: release, calls: &calls,
			},
		},
	}}

	const callers = 8
	results := make(chan streamingFinishResult, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			results <- finish.finish(nil)
		}()
	}
	ready.Wait()
	<-started
	select {
	case result := <-results:
		t.Fatalf("concurrent finish returned before active cleanup completed: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	got := make([]streamingFinishResult, 0, callers)
	for range callers {
		got = append(got, <-results)
	}
	if calls.Load() != 1 {
		t.Fatalf("finalizer calls = %d, want 1", calls.Load())
	}
	for index, result := range got {
		if !errors.Is(result.Err, wantErr) || len(result.Panics) != 1 {
			t.Fatalf("result[%d] = %#v, want cached error and panic", index, result)
		}
		panicErr := result.Panics[0]
		if panicErr.Factory != "blocking" || panicErr.Value != panicValue || len(panicErr.Stack) == 0 {
			t.Fatalf("result[%d] panic = %#v", index, panicErr)
		}
	}
	if got[0].Panics[0] == got[1].Panics[0] || &got[0].Panics[0].Stack[0] == &got[1].Panics[0].Stack[0] {
		t.Fatal("concurrent finish results share mutable panic or stack storage")
	}
}

func TestStreamingFinishAbortSentinelIsCompleteAndCachedForConcurrentCallers(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	order := make([]string, 0, 3)
	var firstCalls atomic.Int32
	var abortCalls atomic.Int32
	var lastCalls atomic.Int32
	finish := &streamingFinish{finalizers: []streamingFinalizerEntry{
		{
			factory: "first", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{name: "first", order: &order, calls: &firstCalls},
		},
		{
			factory: "abort", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{
				name: "abort", order: &order, panic: http.ErrAbortHandler, calls: &abortCalls,
			},
		},
		{
			factory: "last", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{
				name: "last", order: &order, started: started, release: release, calls: &lastCalls,
			},
		},
	}}

	type outcome struct {
		result    streamingFinishResult
		recovered any
	}
	const callers = 8
	outcomes := make(chan outcome, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			got := outcome{}
			func() {
				defer func() { got.recovered = recover() }()
				got.result = finish.finish(nil)
			}()
			outcomes <- got
		}()
	}
	ready.Wait()
	<-started
	select {
	case got := <-outcomes:
		t.Fatalf("concurrent finish returned before active cleanup completed: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	results := make([]streamingFinishResult, 0, callers)
	for range callers {
		got := <-outcomes
		if got.recovered != nil {
			t.Fatalf("finish caller recovered raw panic: %#v", got.recovered)
		}
		if len(got.result.Panics) != 1 || got.result.Panics[0].Value != http.ErrAbortHandler {
			t.Fatalf("finish result = %#v, want cached attributed abort sentinel", got.result)
		}
		results = append(results, got.result)
	}
	if firstCalls.Load() != 1 || abortCalls.Load() != 1 || lastCalls.Load() != 1 {
		t.Fatalf(
			"finalizer calls first/abort/last = %d/%d/%d, want 1/1/1",
			firstCalls.Load(), abortCalls.Load(), lastCalls.Load(),
		)
	}
	if !reflect.DeepEqual(order, []string{"last", "abort", "first"}) {
		t.Fatalf("finish order = %v, want complete reverse cleanup", order)
	}
	if results[0].Panics[0] == results[1].Panics[0] ||
		&results[0].Panics[0].Stack[0] == &results[1].Panics[0].Stack[0] {
		t.Fatal("concurrent finish results share mutable panic or stack storage")
	}
}

func TestStreamingFinishCachesRetainedDownstreamPanicAndCompletesCleanup(t *testing.T) {
	want := &struct{ label string }{label: "retained downstream"}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	order := make([]string, 0, 3)
	var retainedCalls atomic.Int32
	protected := protectStreamingDownstreamWriter(&task10MinimalWriter{writePanic: want})
	finish := &streamingFinish{finalizers: []streamingFinalizerEntry{
		{
			factory: "first", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{name: "first", order: &order},
		},
		{
			factory: "outer", phase: PhaseBodyFilter,
			finalizer: task10RetainedDownstreamFinalizer{
				ResponseWriter: protected,
				call: task10RetainedDownstreamCall{
					name: "retained", order: &order, calls: &retainedCalls,
				},
			},
		},
		{
			factory: "last", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{
				name: "last", order: &order, started: started, release: release,
			},
		},
	}}

	const callers = 8
	results := make(chan streamingFinishResult, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			results <- finish.finish(nil)
		}()
	}
	ready.Wait()
	<-started
	select {
	case result := <-results:
		t.Fatalf("concurrent finish returned before retained cleanup completed: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	for range callers {
		result := <-results
		if result.Err != nil || len(result.Panics) != 0 {
			t.Fatalf("finish result = %#v, want only retained downstream panic", result)
		}
		recovered := captureTask10Panic(func() { panicFirstStreamingFinish(result) })
		if recovered != want {
			t.Fatalf("cached downstream panic = %#v, want original %#v", recovered, want)
		}
	}
	if retainedCalls.Load() != 1 {
		t.Fatalf("retained downstream calls = %d, want 1", retainedCalls.Load())
	}
	if !reflect.DeepEqual(order, []string{"last", "retained", "first"}) {
		t.Fatalf("finish order = %v, want complete reverse order", order)
	}
}

func TestStreamingFinishPreservesDirectExistingPanicAttribution(t *testing.T) {
	want := &PanicError{Factory: "inner", Phase: PhaseProtocol, Value: errors.New("inner panic")}
	result := (&streamingFinish{finalizers: []streamingFinalizerEntry{
		{
			factory: "outer", phase: PhaseBodyFilter,
			finalizer: task10StreamingFinalizer{panic: want},
		},
	}}).finish(nil)
	if len(result.Panics) != 1 {
		t.Fatalf("finish result = %#v, want one existing panic", result)
	}
	got := result.Panics[0]
	if got == want || got.Factory != want.Factory || got.Phase != want.Phase || got.Value != want.Value {
		t.Fatalf("existing panic = %#v, want detached canonical %#v", got, want)
	}
}

var (
	_ io.Closer                       = task10StreamingCloser{}
	_ base.StreamingResponseFinalizer = task10StreamingFinalizer{}
)

func resolvedPlan16Binding(t *testing.T, factory string, p Plugin, id string) Binding {
	t.Helper()
	binding, err := BindPluginChecked(
		factory,
		p,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: id},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", factory, err)
	}
	return binding
}

func TestStreamingExecutorSkipsHeaderFilterWrapperWithoutBindings(t *testing.T) {
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	wrapped := executor.wrapStreamingHeaderFilters(response, request)
	if wrapped != response {
		t.Fatalf("empty header-filter wrapper = %T, want original %T", wrapped, response)
	}
}

type plan16CloseWriter struct {
	http.ResponseWriter
	closes   *int
	finishes *int
}

func (w plan16CloseWriter) Close() error {
	*w.closes = *w.closes + 1
	return nil
}

func (w plan16CloseWriter) FinishStreamingResponse(error) error {
	if w.finishes != nil {
		*w.finishes = *w.finishes + 1
	}
	return nil
}

func (p *plan16StreamingPlugin) Init() error                            { return nil }
func (p *plan16StreamingPlugin) PostInit() error                        { return nil }
func (p *plan16StreamingPlugin) Config() any                            { return nil }
func (p *plan16StreamingPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *plan16StreamingPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	if p.closes != nil {
		return plan16CloseWriter{ResponseWriter: w, closes: p.closes, finishes: p.finishes}, nil
	}
	return w, nil
}

func (p *plan16StreamingPlugin) ResponseCapability() ResponseCapability {
	return ResponseCapability{StreamingBodyFilter: true}
}

func TestStreamingExecutorRunsWrapperAndPreservesSource(t *testing.T) {
	p := &plan16StreamingPlugin{}
	p.Name = "streaming-test"
	binding := resolvedPlan16Binding(t, "proxy-buffering", p, "route")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	req = apisixctx.WithRequestLifecycle(req, lifecycle)
	lifecycle.SetFinalRequest(req)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	executor.Then(handler).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("streaming response = %d/%q", recorder.Code, recorder.Body.String())
	}
	if source := lifecycle.ResponseSource(); source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("streaming response source = %q, want upstream", source)
	}
}

func TestStreamingExecutorFinishesWrapperExactlyOnceOnNormalCompletion(t *testing.T) {
	closes, finishes := 0, 0
	p := &plan16StreamingPlugin{closes: &closes, finishes: &finishes}
	p.Name = "streaming-finish"
	binding := resolvedPlan16Binding(t, "proxy-buffering", p, "finish-route")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	handler := executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if closes != 1 || finishes != 1 {
		t.Fatalf("finish counts = closes:%d finalizers:%d, want 1/1", closes, finishes)
	}
}

type plan16ProtocolPlugin struct {
	plan16StreamingPlugin
	disposition base.ProtocolDisposition
}

type plan16LateSourceProtocolPlugin struct{ plan16ProtocolPlugin }

func (p *plan16LateSourceProtocolPlugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	_ http.Handler,
) (base.ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error) {
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("too-late"))
	return base.ProtocolResponded, r, apisixctx.ResponseSourceAPISIX, nil
}

type task10ProtocolPanicPlugin struct {
	plan16ProtocolPlugin
	beforePanic any
	afterPanic  any
	callNext    bool
	returnErr   error
}

func (p *task10ProtocolPanicPlugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error) {
	if p.beforePanic != nil {
		panic(p.beforePanic)
	}
	if p.callNext && next != nil {
		next.ServeHTTP(w, r)
	}
	if p.afterPanic != nil {
		panic(p.afterPanic)
	}
	if p.returnErr != nil {
		return base.ProtocolResponded, r, apisixctx.ResponseSourceUnknown, p.returnErr
	}
	return base.ProtocolResponded, r, apisixctx.ResponseSourceAPISIX, nil
}

type plan16CompressionPlugin struct {
	base.BasePlugin
	coding        compression.Coding
	rank          int
	eligible      func(compression.ResponseMeta) bool
	registerCall  *int
	wrapCall      *int
	registerPanic any
	wrapPanic     any
	finalizer     base.StreamingResponseFinalizer
	finishCall    *task10RetainedDownstreamCall
}

type plan16BarePlugin struct{ base.BasePlugin }

func (p *plan16CompressionPlugin) Init() error                            { return nil }
func (p *plan16CompressionPlugin) PostInit() error                        { return nil }
func (p *plan16CompressionPlugin) Config() any                            { return nil }
func (p *plan16CompressionPlugin) Handler(next http.Handler) http.Handler { return next }

func (p *plan16BarePlugin) Init() error                            { return nil }
func (p *plan16BarePlugin) PostInit() error                        { return nil }
func (p *plan16BarePlugin) Config() any                            { return nil }
func (p *plan16BarePlugin) Handler(next http.Handler) http.Handler { return next }

func (p *plan16CompressionPlugin) RunStreamingHeaderFilter(_ *http.Request, _ *base.StreamingResponseState) error {
	return nil
}

func (p *plan16CompressionPlugin) RegisterCompressionOffers(*http.Request, *compression.State) []compression.Offer {
	if p.registerPanic != nil {
		panic(p.registerPanic)
	}
	if p.registerCall != nil {
		*p.registerCall++
	}
	return []compression.Offer{{Coding: p.coding, Rank: p.rank, Eligible: p.eligible}}
}

func (p *plan16CompressionPlugin) WrapCompression(
	w http.ResponseWriter,
	_ *http.Request,
	_ *compression.State,
	_ compression.Decision,
) (http.ResponseWriter, error) {
	if p.wrapPanic != nil {
		panic(p.wrapPanic)
	}
	if p.wrapCall != nil {
		*p.wrapCall++
	}
	if p.finalizer != nil {
		return &task10FinalizingWriter{ResponseWriter: w, finalizer: p.finalizer}, nil
	}
	if p.finishCall != nil {
		return task10RetainedDownstreamFinalizer{ResponseWriter: w, call: *p.finishCall}, nil
	}
	return w, nil
}

var _ CompressionOfferPlugin = (*plan16CompressionPlugin)(nil)

type task10HeaderPanicPlugin struct {
	base.BasePlugin
	panicValue any
}

func (*task10HeaderPanicPlugin) Init() error                            { return nil }
func (*task10HeaderPanicPlugin) PostInit() error                        { return nil }
func (*task10HeaderPanicPlugin) Config() any                            { return nil }
func (*task10HeaderPanicPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *task10HeaderPanicPlugin) RunStreamingHeaderFilter(
	*http.Request,
	*base.StreamingResponseState,
) error {
	panic(p.panicValue)
}

type task10BodyPanicPlugin struct {
	base.BasePlugin
	panicValue any
	returned   http.ResponseWriter
}

type task10FinalizingWriter struct {
	http.ResponseWriter
	finalizer base.StreamingResponseFinalizer
}

func (w *task10FinalizingWriter) FinishStreamingResponse(cause error) error {
	return w.finalizer.FinishStreamingResponse(cause)
}

type task10FinalizerBodyPlugin struct {
	base.BasePlugin
	finalizer base.StreamingResponseFinalizer
}

type task10RetainedDownstreamCloserPlugin struct {
	base.BasePlugin
	call task10RetainedDownstreamCall
}

func (*task10RetainedDownstreamCloserPlugin) Init() error     { return nil }
func (*task10RetainedDownstreamCloserPlugin) PostInit() error { return nil }
func (*task10RetainedDownstreamCloserPlugin) Config() any     { return nil }
func (*task10RetainedDownstreamCloserPlugin) Handler(next http.Handler) http.Handler {
	return next
}

func (p *task10RetainedDownstreamCloserPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	return task10RetainedDownstreamCloser{ResponseWriter: w, call: p.call}, nil
}

func (*task10RetainedDownstreamCloserPlugin) ResponseCapability() ResponseCapability {
	return ResponseCapability{StreamingBodyFilter: true}
}

func (*task10FinalizerBodyPlugin) Init() error                            { return nil }
func (*task10FinalizerBodyPlugin) PostInit() error                        { return nil }
func (*task10FinalizerBodyPlugin) Config() any                            { return nil }
func (*task10FinalizerBodyPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *task10FinalizerBodyPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	return &task10FinalizingWriter{ResponseWriter: w, finalizer: p.finalizer}, nil
}

func (*task10BodyPanicPlugin) Init() error                            { return nil }
func (*task10BodyPanicPlugin) PostInit() error                        { return nil }
func (*task10BodyPanicPlugin) Config() any                            { return nil }
func (*task10BodyPanicPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *task10BodyPanicPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	if p.returned != nil {
		return p.returned, nil
	}
	return w, nil
}

type task10ModeConfig struct{}

func (task10ModeConfig) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{
		Modes: base.ResponseModeBounded | base.ResponseModeStreaming,
	}, nil
}

type task10ModePanicPlugin struct {
	base.BasePlugin
	panicValue any
}

func (*task10ModePanicPlugin) Init() error                            { return nil }
func (*task10ModePanicPlugin) PostInit() error                        { return nil }
func (*task10ModePanicPlugin) Config() any                            { return task10ModeConfig{} }
func (*task10ModePanicPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *task10ModePanicPlugin) SelectResponseMode(*http.Request) base.RequestResponseMode {
	panic(p.panicValue)
}

func (*task10ModePanicPlugin) RunBufferedBodyFilter(*http.Request, *base.ResponseState) error {
	return nil
}

func (*task10ModePanicPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	return w, nil
}

type task10MinimalWriter struct {
	header     http.Header
	writePanic any
}

func (w *task10MinimalWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*task10MinimalWriter) WriteHeader(int) {}

func (w *task10MinimalWriter) Write(body []byte) (int, error) {
	if w.writePanic != nil {
		panic(w.writePanic)
	}
	return len(body), nil
}

type task10AllOptionalWriter struct{ task10MinimalWriter }

func (*task10AllOptionalWriter) Flush()                   {}
func (*task10AllOptionalWriter) FlushError() error        { return nil }
func (*task10AllOptionalWriter) CloseNotify() <-chan bool { return make(chan bool) }
func (*task10AllOptionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

func (*task10AllOptionalWriter) ReadFrom(reader io.Reader) (int64, error) {
	return io.Copy(io.Discard, reader)
}
func (*task10AllOptionalWriter) SetReadDeadline(time.Time) error  { return nil }
func (*task10AllOptionalWriter) SetWriteDeadline(time.Time) error { return nil }
func (*task10AllOptionalWriter) EnableFullDuplex() error          { return nil }
func (*task10AllOptionalWriter) Push(string, *http.PushOptions) error {
	return nil
}
func (*task10AllOptionalWriter) WriteString(value string) (int, error) { return len(value), nil }

type task10PanicOptionalWriter struct{}

func (w *task10PanicOptionalWriter) Header() http.Header {
	panic("Header")
}
func (*task10PanicOptionalWriter) WriteHeader(int)           { panic("WriteHeader") }
func (*task10PanicOptionalWriter) Write([]byte) (int, error) { panic("Write") }
func (*task10PanicOptionalWriter) Flush()                    { panic("Flush") }
func (*task10PanicOptionalWriter) FlushError() error         { panic("FlushError") }
func (*task10PanicOptionalWriter) CloseNotify() <-chan bool  { panic("CloseNotify") }
func (*task10PanicOptionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	panic("Hijack")
}
func (*task10PanicOptionalWriter) ReadFrom(io.Reader) (int64, error) { panic("ReadFrom") }
func (*task10PanicOptionalWriter) SetReadDeadline(time.Time) error   { panic("SetReadDeadline") }
func (*task10PanicOptionalWriter) SetWriteDeadline(time.Time) error  { panic("SetWriteDeadline") }
func (*task10PanicOptionalWriter) EnableFullDuplex() error           { panic("EnableFullDuplex") }
func (*task10PanicOptionalWriter) Push(string, *http.PushOptions) error {
	panic("Push")
}
func (*task10PanicOptionalWriter) WriteString(string) (int, error) { panic("WriteString") }

func captureTask10Panic(call func()) (recovered any) {
	defer func() { recovered = recover() }()
	call()
	return nil
}

func requireTask10StreamingPanic(t *testing.T, recovered any, factory, operation string) {
	t.Helper()
	panicErr, ok := recovered.(*PanicError)
	if !ok {
		t.Fatalf("%s panic = %T(%v), want *PanicError", operation, recovered, recovered)
	}
	if panicErr.Factory != factory || panicErr.Phase != PhaseBodyFilter || panicErr.Value != operation ||
		len(panicErr.Stack) == 0 {
		t.Fatalf(
			"%s panic = %#v, want factory=%q phase=%q value=%q",
			operation,
			panicErr,
			factory,
			PhaseBodyFilter,
			operation,
		)
	}
}

func TestStreamingHeaderPanicIsAttributed(t *testing.T) {
	plugin := &task10HeaderPanicPlugin{panicValue: errors.New("header panic")}
	plugin.Name = "cors"
	binding := resolvedPlan16Binding(t, "cors", plugin, "header-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	err = executor.runHeaderFilters(
		httptest.NewRequest(http.MethodGet, "/", nil),
		&base.StreamingResponseState{Status: http.StatusOK, Header: make(http.Header)},
	)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "cors" || panicErr.Phase != PhaseHeaderFilter ||
		panicErr.Value != plugin.panicValue {
		t.Fatalf("header error = %#v, want attributed panic", err)
	}
}

func TestStreamingModeSelectorPanicIsAttributed(t *testing.T) {
	plugin := &task10ModePanicPlugin{panicValue: errors.New("selector panic")}
	plugin.Name = "ai-rate-limiting"
	binding := resolvedPlan16Binding(t, "ai-rate-limiting", plugin, "selector-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	_, err = executor.wrapBody(
		&task10MinimalWriter{}, httptest.NewRequest(http.MethodGet, "/", nil), &streamingFinish{},
	)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "ai-rate-limiting" ||
		panicErr.Phase != PhaseBodyFilter || panicErr.Value != plugin.panicValue {
		t.Fatalf("selector error = %#v, want attributed panic", err)
	}
}

func TestStreamingResponseWrapperConstructionPanicIsAttributed(t *testing.T) {
	plugin := &task10BodyPanicPlugin{panicValue: errors.New("wrapper panic")}
	plugin.Name = "proxy-buffering"
	binding := resolvedPlan16Binding(t, "proxy-buffering", plugin, "wrapper-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	_, err = executor.wrapBody(
		&task10MinimalWriter{}, httptest.NewRequest(http.MethodGet, "/", nil), &streamingFinish{},
	)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "proxy-buffering" ||
		panicErr.Phase != PhaseBodyFilter || panicErr.Value != plugin.panicValue {
		t.Fatalf("wrapper error = %#v, want attributed panic", err)
	}
}

func TestCompressionRegistrationPanicIsAttributed(t *testing.T) {
	plugin := &plan16CompressionPlugin{coding: compression.Gzip, rank: 1, registerPanic: errors.New("register panic")}
	plugin.Name = "gzip"
	binding := resolvedPlan16Binding(t, "gzip", plugin, "register-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	_, _, err = executor.registerCompressionOffers(httptest.NewRequest(http.MethodGet, "/", nil))
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "gzip" || panicErr.Phase != PhaseBodyFilter ||
		panicErr.Value != plugin.registerPanic {
		t.Fatalf("registration error = %#v, want attributed panic", err)
	}
}

func TestCompressionWrapperPanicIsAttributed(t *testing.T) {
	plugin := &plan16CompressionPlugin{coding: compression.Gzip, rank: 1, wrapPanic: errors.New("wrap panic")}
	plugin.Name = "gzip"
	binding := resolvedPlan16Binding(t, "gzip", plugin, "wrap-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	request, negotiation, err := executor.registerCompressionOffers(request)
	if err != nil {
		t.Fatalf("registerCompressionOffers() error = %v", err)
	}
	_, err = (&streamingFinish{compression: negotiation}).applyCompression(
		&task10MinimalWriter{}, request, http.StatusOK, make(http.Header),
	)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "gzip" || panicErr.Phase != PhaseBodyFilter ||
		panicErr.Value != plugin.wrapPanic {
		t.Fatalf("compression wrapper error = %#v, want attributed panic", err)
	}
}

func TestStreamingReturnedWriterPanicsAreAttributedForEveryOperation(t *testing.T) {
	plugin := &task10BodyPanicPlugin{returned: &task10PanicOptionalWriter{}}
	plugin.Name = "proxy-buffering"
	binding := resolvedPlan16Binding(t, "proxy-buffering", plugin, "writer-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	w, err := executor.wrapBody(
		&task10MinimalWriter{}, httptest.NewRequest(http.MethodGet, "/", nil), &streamingFinish{},
	)
	if err != nil {
		t.Fatalf("wrapBody() error = %v", err)
	}
	tests := []struct {
		name string
		call func()
	}{
		{"Header", func() { _ = w.Header() }},
		{"WriteHeader", func() { w.WriteHeader(http.StatusOK) }},
		{"Write", func() { _, _ = w.Write(nil) }},
		{"WriteString", func() { _, _ = io.WriteString(w, "x") }},
		{"ReadFrom", func() { _, _ = w.(io.ReaderFrom).ReadFrom(strings.NewReader("x")) }},
		{"Flush", func() { w.(http.Flusher).Flush() }},
		{"FlushError", func() { _ = w.(interface{ FlushError() error }).FlushError() }},
		{"Hijack", func() { _, _, _ = w.(http.Hijacker).Hijack() }},
		{"CloseNotify", func() { _ = w.(interface{ CloseNotify() <-chan bool }).CloseNotify() }},
		{"Push", func() { _ = w.(http.Pusher).Push("/x", nil) }},
		{"SetReadDeadline", func() { _ = http.NewResponseController(w).SetReadDeadline(time.Now()) }},
		{"SetWriteDeadline", func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Now()) }},
		{"EnableFullDuplex", func() { _ = http.NewResponseController(w).EnableFullDuplex() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireTask10StreamingPanic(t, captureTask10Panic(test.call), "proxy-buffering", test.name)
		})
	}
}

func TestStreamingWriterGuardPreservesExactOptionalInterfaceSet(t *testing.T) {
	plugin := &task10BodyPanicPlugin{}
	plugin.Name = "proxy-buffering"
	binding := resolvedPlan16Binding(t, "proxy-buffering", plugin, "optional-set")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	tests := []struct {
		name       string
		downstream http.ResponseWriter
		want       bool
	}{
		{name: "unsupported", downstream: &task10MinimalWriter{}, want: false},
		{name: "supported", downstream: &task10AllOptionalWriter{}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wrapped, err := executor.wrapBody(
				test.downstream, httptest.NewRequest(http.MethodGet, "/", nil), &streamingFinish{},
			)
			if err != nil {
				t.Fatalf("wrapBody() error = %v", err)
			}
			got := []bool{
				implements[http.Flusher](wrapped),
				implements[interface{ FlushError() error }](wrapped),
				implements[interface{ CloseNotify() <-chan bool }](wrapped),
				implements[http.Hijacker](wrapped),
				implements[io.ReaderFrom](wrapped),
				implements[interface {
					SetReadDeadline(time.Time) error
					SetWriteDeadline(time.Time) error
				}](wrapped),
				implements[interface{ EnableFullDuplex() error }](wrapped),
				implements[http.Pusher](wrapped),
				implements[io.StringWriter](wrapped),
			}
			for index, supported := range got {
				if supported != test.want {
					t.Fatalf("optional capability[%d] = %t, want %t (writer=%T)", index, supported, test.want, wrapped)
				}
			}
		})
	}
}

func TestStreamingWriterGuardPreservesRawDownstreamPanicIdentity(t *testing.T) {
	want := errors.New("downstream writer panic")
	plugin := &task10BodyPanicPlugin{}
	plugin.Name = "proxy-buffering"
	binding := resolvedPlan16Binding(t, "proxy-buffering", plugin, "downstream-panic")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	w, err := executor.wrapBody(
		&task10MinimalWriter{writePanic: want},
		httptest.NewRequest(http.MethodGet, "/", nil),
		&streamingFinish{},
	)
	if err != nil {
		t.Fatalf("wrapBody() error = %v", err)
	}
	recovered := captureTask10Panic(func() { _, _ = w.Write(nil) })
	if recovered != want {
		t.Fatalf("downstream panic = %#v, want original %#v", recovered, want)
	}
}

func TestStreamingFinishPreservesRetainedDownstreamPanicIdentity(t *testing.T) {
	panicValues := []struct {
		name  string
		value any
	}{
		{name: "raw pointer", value: &struct{ label string }{label: "raw downstream"}},
		{name: "abort sentinel", value: http.ErrAbortHandler},
		{name: "existing panic error", value: &PanicError{
			Factory: "downstream", Phase: PhaseProtocol, Value: errors.New("existing downstream panic"),
		}},
	}
	layers := []struct {
		name  string
		build func(*testing.T, *task10RetainedDownstreamCall) *StreamingResponseExecutor
		next  http.Handler
	}{
		{
			name: "body closer",
			build: func(t *testing.T, call *task10RetainedDownstreamCall) *StreamingResponseExecutor {
				plugin := &task10RetainedDownstreamCloserPlugin{call: *call}
				plugin.Name = "proxy-buffering"
				executor, err := NewStreamingResponseExecutor([]Binding{
					resolvedPlan16Binding(t, "proxy-buffering", plugin, "retained-body"),
				})
				if err != nil {
					t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
				}
				return executor
			},
			next: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		},
		{
			name: "compression finalizer",
			build: func(t *testing.T, call *task10RetainedDownstreamCall) *StreamingResponseExecutor {
				plugin := &plan16CompressionPlugin{coding: compression.Gzip, rank: 1, finishCall: call}
				plugin.Name = "gzip"
				executor, err := NewStreamingResponseExecutor([]Binding{
					resolvedPlan16Binding(t, "gzip", plugin, "retained-compression"),
				})
				if err != nil {
					t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
				}
				return executor
			},
			next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		},
	}
	for _, layer := range layers {
		for _, panicValue := range panicValues {
			t.Run(layer.name+"/"+panicValue.name, func(t *testing.T) {
				var calls atomic.Int32
				call := &task10RetainedDownstreamCall{calls: &calls}
				executor := layer.build(t, call)
				request := httptest.NewRequest(http.MethodGet, "/", nil)
				request.Header.Set("Accept-Encoding", "gzip")
				recovered := captureTask10Panic(func() {
					executor.Then(layer.next).ServeHTTP(
						&task10MinimalWriter{writePanic: panicValue.value},
						request,
					)
				})
				if recovered != panicValue.value {
					t.Fatalf("finish panic = %T(%v), want exact retained downstream %T(%v)",
						recovered, recovered, panicValue.value, panicValue.value)
				}
				if calls.Load() != 1 {
					t.Fatalf("retained downstream calls = %d, want 1", calls.Load())
				}
			})
		}
	}
}

func TestStreamingTerminalAndCommitPanicPrecedeRetainedDownstreamFinishPanic(t *testing.T) {
	finishValue := &struct{ label string }{label: "finish downstream"}
	primary := &struct{ label string }{label: "primary"}
	plugin := &task10RetainedDownstreamCloserPlugin{}
	plugin.Name = "proxy-buffering"
	executor, err := NewStreamingResponseExecutor([]Binding{
		resolvedPlan16Binding(t, "proxy-buffering", plugin, "finish-primary"),
	})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	t.Run("terminal", func(t *testing.T) {
		recovered := captureTask10Panic(func() {
			executor.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic(primary)
			})).ServeHTTP(
				&task10MinimalWriter{writePanic: finishValue},
				httptest.NewRequest(http.MethodGet, "/", nil),
			)
		})
		if recovered != primary {
			t.Fatalf("terminal panic = %#v, want primary %#v", recovered, primary)
		}
	})
	t.Run("commit", func(t *testing.T) {
		state := &base.ResponseState{Status: http.StatusOK, Header: make(http.Header)}
		recovered := captureTask10Panic(func() {
			_ = executor.CommitResponse(
				&task10MinimalWriter{writePanic: finishValue},
				httptest.NewRequest(http.MethodGet, "/", nil),
				state,
				func(http.ResponseWriter, *base.ResponseState) { panic(primary) },
			)
		})
		if recovered != primary {
			t.Fatalf("commit panic = %#v, want primary %#v", recovered, primary)
		}
	})
}

func task10ProtocolExecutor(t *testing.T, terminal base.ExclusiveProtocolTerminal) *StreamingResponseExecutor {
	t.Helper()
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	executor, err = executor.WithRouteTerminals([]RouteTerminalCandidate{{
		Identity: "ai-proxy", Protocol: ProtocolAI, Scope: ScopeRoute,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "protocol-panic"},
		Terminal:   terminal,
	}})
	if err != nil {
		t.Fatalf("WithRouteTerminals() error = %v", err)
	}
	return executor
}

func TestProtocolPanicBeforeNextIsAttributed(t *testing.T) {
	want := errors.New("before next")
	terminal := &task10ProtocolPanicPlugin{beforePanic: want}
	terminal.Name = "terminal"
	_, _, err := task10ProtocolExecutor(t, terminal).RunExclusiveProtocol(
		&task10MinimalWriter{}, httptest.NewRequest(http.MethodGet, "/", nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	)
	var panicErr *PanicError
	if !errors.As(err, &panicErr) || panicErr.Factory != "ai-proxy" || panicErr.Phase != PhaseProtocol ||
		panicErr.Value != want {
		t.Fatalf("protocol error = %#v, want attributed before-next panic", err)
	}
}

func TestProtocolPanicAfterNextIsAttributed(t *testing.T) {
	want := errors.New("after next")
	terminal := &task10ProtocolPanicPlugin{callNext: true, afterPanic: want}
	terminal.Name = "terminal"
	nextCalls := 0
	_, _, err := task10ProtocolExecutor(t, terminal).RunExclusiveProtocol(
		&task10MinimalWriter{},
		httptest.NewRequest(http.MethodGet, "/", nil),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ }),
	)
	var panicErr *PanicError
	if nextCalls != 1 || !errors.As(err, &panicErr) || panicErr.Factory != "ai-proxy" ||
		panicErr.Phase != PhaseProtocol || panicErr.Value != want {
		t.Fatalf("protocol next/error = %d/%#v, want one next and attributed after-next panic", nextCalls, err)
	}
}

func TestProtocolNextPanicEscapesUnchanged(t *testing.T) {
	want := errors.New("raw downstream panic")
	terminal := &task10ProtocolPanicPlugin{callNext: true}
	terminal.Name = "terminal"
	recovered := captureTask10Panic(func() {
		_, _, _ = task10ProtocolExecutor(t, terminal).RunExclusiveProtocol(
			&task10MinimalWriter{},
			httptest.NewRequest(http.MethodGet, "/", nil),
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) }),
		)
	})
	if recovered != want {
		t.Fatalf("next panic = %#v, want original %#v", recovered, want)
	}
}

func TestStreamingProtocolPanicUsesStableFinishCause(t *testing.T) {
	tests := []struct {
		name     string
		before   bool
		want     error
		wantNext int
	}{
		{name: "before next", before: true, want: errors.New("sensitive before-next panic")},
		{name: "after next", want: errors.New("sensitive after-next panic"), wantNext: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var cause error
			finishPanic := errors.New("finish panic")
			executor := task10FinalizerExecutor(t, task10StreamingFinalizer{
				cause: &cause,
				panic: finishPanic,
			})
			terminal := &task10ProtocolPanicPlugin{callNext: !test.before}
			if test.before {
				terminal.beforePanic = test.want
			} else {
				terminal.afterPanic = test.want
			}
			terminal.Name = "terminal"
			var err error
			executor, err = executor.WithRouteTerminals([]RouteTerminalCandidate{{
				Identity: "ai-proxy", Protocol: ProtocolAI, Scope: ScopeRoute,
				Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "stable-finish-cause"},
				Terminal:   terminal,
			}})
			if err != nil {
				t.Fatalf("WithRouteTerminals() error = %v", err)
			}
			nextCalls := 0
			recovered := captureTask10Panic(func() {
				executor.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					nextCalls++
				})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			})
			panicErr, ok := recovered.(*PanicError)
			if !ok || panicErr.Factory != "ai-proxy" || panicErr.Phase != PhaseProtocol ||
				panicErr.Value != test.want {
				t.Fatalf("protocol panic = %#v, want original attributed panic", recovered)
			}
			if nextCalls != test.wantNext {
				t.Fatalf("next calls = %d, want %d", nextCalls, test.wantNext)
			}
			if cause != errStreamingPanic {
				t.Fatalf("finish cause = %#v, want stable errStreamingPanic", cause)
			}
			if panicErr.Value == finishPanic {
				t.Fatal("finish panic replaced the protocol panic")
			}
		})
	}
}

func task10FinalizerExecutor(
	t *testing.T,
	finalizer base.StreamingResponseFinalizer,
) *StreamingResponseExecutor {
	t.Helper()
	plugin := &task10FinalizerBodyPlugin{finalizer: finalizer}
	plugin.Name = "proxy-buffering"
	binding := resolvedPlan16Binding(t, "proxy-buffering", plugin, "finish-precedence")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	return executor
}

func TestStreamingFinishPanicIsPrimaryWithoutTerminalPanic(t *testing.T) {
	want := errors.New("finish panic")
	executor := task10FinalizerExecutor(t, task10StreamingFinalizer{panic: want})
	recovered := captureTask10Panic(func() {
		executor.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	})
	panicErr, ok := recovered.(*PanicError)
	if !ok || panicErr.Factory != "proxy-buffering" || panicErr.Phase != PhaseBodyFilter || panicErr.Value != want {
		t.Fatalf("finish panic = %#v, want attributed primary panic", recovered)
	}
}

func TestStreamingFinishPanicWinsOverOrdinaryProtocolError(t *testing.T) {
	want := errors.New("finish panic")
	executor := task10FinalizerExecutor(t, task10StreamingFinalizer{panic: want})
	terminal := &task10ProtocolPanicPlugin{returnErr: errors.New("protocol error")}
	terminal.Name = "terminal"
	var err error
	executor, err = executor.WithRouteTerminals([]RouteTerminalCandidate{{
		Identity: "ai-proxy", Protocol: ProtocolAI, Scope: ScopeRoute,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "finish-precedence"},
		Terminal:   terminal,
	}})
	if err != nil {
		t.Fatalf("WithRouteTerminals() error = %v", err)
	}
	recovered := captureTask10Panic(func() {
		executor.Then(nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
	panicErr, ok := recovered.(*PanicError)
	if !ok || panicErr.Factory != "proxy-buffering" || panicErr.Value != want {
		t.Fatalf("finish panic = %#v, want finish panic ahead of ordinary protocol error", recovered)
	}
}

func TestStreamingTerminalPanicPreservesIdentityAndUsesStableFinishCause(t *testing.T) {
	want := errors.New("sensitive terminal panic")
	var cause error
	executor := task10FinalizerExecutor(t, task10StreamingFinalizer{cause: &cause})
	recovered := captureTask10Panic(func() {
		executor.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(want) })).ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	})
	if recovered != want {
		t.Fatalf("terminal panic = %#v, want original %#v", recovered, want)
	}
	if cause != errStreamingPanic {
		t.Fatalf("finish cause = %#v, want stable errStreamingPanic", cause)
	}
	if strings.Contains(cause.Error(), "sensitive terminal panic") {
		t.Fatalf("finish cause disclosed raw panic: %q", cause)
	}
}

func TestStreamingOrdinaryFinishErrorKeepsCommitPolicy(t *testing.T) {
	want := errors.New("finish error")
	t.Run("precommit stable error", func(t *testing.T) {
		executor := task10FinalizerExecutor(t, task10StreamingFinalizer{err: want})
		response := httptest.NewRecorder()
		recovered := captureTask10Panic(func() {
			executor.Then(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
				response,
				httptest.NewRequest(http.MethodGet, "/", nil),
			)
		})
		if recovered != nil || response.Code != http.StatusInternalServerError {
			t.Fatalf("precommit finish = panic:%#v status:%d, want stable 500", recovered, response.Code)
		}
	})
	t.Run("postcommit abort", func(t *testing.T) {
		executor := task10FinalizerExecutor(t, task10StreamingFinalizer{err: want})
		recovered := captureTask10Panic(func() {
			executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("committed"))
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		})
		if recovered != http.ErrAbortHandler {
			t.Fatalf("postcommit finish panic = %#v, want http.ErrAbortHandler", recovered)
		}
	})
}

func TestStreamingCommitPanicPreservesIdentityAndUsesStableFinishCause(t *testing.T) {
	want := errors.New("sensitive commit panic")
	var cause error
	executor := task10FinalizerExecutor(t, task10StreamingFinalizer{cause: &cause})
	state := &base.ResponseState{Status: http.StatusOK, Header: make(http.Header)}
	recovered := captureTask10Panic(func() {
		_ = executor.CommitResponse(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
			state,
			func(http.ResponseWriter, *base.ResponseState) { panic(want) },
		)
	})
	if recovered != want || cause != errStreamingPanic {
		t.Fatalf("commit panic/cause = %#v/%#v, want original/stable sentinel", recovered, cause)
	}
}

func TestStreamingAdditionalFinishPanicIsLoggedWithoutReplacingPrimary(t *testing.T) {
	bodyValue := errors.New("body secret")
	compressionValue := errors.New("compression secret")
	bodyFinalizer := task10StreamingFinalizer{panic: bodyValue}
	compressionFinalizer := task10StreamingFinalizer{panic: compressionValue}
	bodyPlugin := &task10FinalizerBodyPlugin{finalizer: bodyFinalizer}
	bodyPlugin.Name = "proxy-buffering"
	compressionPlugin := &plan16CompressionPlugin{
		coding: compression.Gzip, rank: 1, finalizer: compressionFinalizer,
	}
	compressionPlugin.Name = "gzip"
	executor, err := NewStreamingResponseExecutor([]Binding{
		resolvedPlan16Binding(t, "proxy-buffering", bodyPlugin, "body-finish"),
		resolvedPlan16Binding(t, "gzip", compressionPlugin, "compression-finish"),
	})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	observed := make(chan logger.Entry, 1)
	stopObserver := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		if strings.Contains(entry.Message, "additional streaming finish panic") {
			observed <- entry
		}
	})
	t.Cleanup(stopObserver)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recovered := captureTask10Panic(func() {
		executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("committed"))
		})).ServeHTTP(httptest.NewRecorder(), request)
	})
	panicErr, ok := recovered.(*PanicError)
	if !ok || panicErr.Factory != "gzip" || panicErr.Value != compressionValue {
		t.Fatalf("primary finish panic = %#v, want last-registered compression finalizer panic", recovered)
	}
	select {
	case entry := <-observed:
		if !strings.Contains(entry.Message, `factory="proxy-buffering"`) ||
			!strings.Contains(entry.Message, `phase="body_filter"`) ||
			strings.Contains(entry.Message, "body secret") {
			t.Fatalf("additional finish log = %q", entry.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("additional finish panic was not logged")
	}
}

func TestStreamingExecutorRegistersCompressionAndWrapsOnlyFrozenWinner(t *testing.T) {
	gzipRegisters, gzipWraps := 0, 0
	brRegisters, brWraps := 0, 0
	gzipPlugin := &plan16CompressionPlugin{
		coding: compression.Gzip, rank: 1, registerCall: &gzipRegisters, wrapCall: &gzipWraps,
	}
	brPlugin := &plan16CompressionPlugin{
		coding: compression.Brotli, rank: 2, registerCall: &brRegisters, wrapCall: &brWraps,
	}
	gzipPlugin.Name, brPlugin.Name = "gzip", "brotli"
	executor, err := NewStreamingResponseExecutor([]Binding{
		resolvedPlan16Binding(t, "gzip", gzipPlugin, "gzip"),
		resolvedPlan16Binding(t, "brotli", brPlugin, "br"),
	})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0.8, br;q=1")
	state := &base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/plain"}},
		Body:   []byte("ok"),
	}
	recorder := httptest.NewRecorder()
	if err := executor.CommitResponse(recorder, request, state, func(w http.ResponseWriter, state *base.ResponseState) {
		w.WriteHeader(state.Status)
		_, _ = w.Write(state.Body)
	}); err != nil {
		t.Fatalf("CommitResponse() error = %v", err)
	}
	if gzipRegisters != 1 || brRegisters != 1 {
		t.Fatalf("compression registration counts = gzip:%d br:%d, want one each", gzipRegisters, brRegisters)
	}
	if gzipWraps != 0 || brWraps != 1 {
		t.Fatalf("compression wrapper counts = gzip:%d br:%d, want only br winner", gzipWraps, brWraps)
	}
}

func TestStreamingExecutorDefersCompressionDecisionUntilFinalStatus(t *testing.T) {
	wraps := 0
	plugin := &plan16CompressionPlugin{
		coding:   compression.Gzip,
		rank:     1,
		wrapCall: &wraps,
	}
	plugin.Name = "gzip"
	executor, err := NewStreamingResponseExecutor([]Binding{
		resolvedPlan16Binding(t, "gzip", plugin, "gzip-204"),
	})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(recorder, request)
	if wraps != 0 {
		t.Fatalf("compression wrapper calls = %d, want 0 for final 204", wraps)
	}
}

func TestStreamingExecutorRejectsUnacceptableEncodingWithBodyless406(t *testing.T) {
	plugin := &plan16CompressionPlugin{coding: compression.Gzip, rank: 1}
	plugin.Name = "gzip"
	executor, err := NewStreamingResponseExecutor([]Binding{
		resolvedPlan16Binding(t, "gzip", plugin, "gzip-406"),
	})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	for _, acceptEncoding := range []string{"*;q=0", "gzip;q=0, identity;q=0"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", acceptEncoding)
			response := httptest.NewRecorder()
			executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "7")
				w.Header().Set("Content-MD5", "stale")
				_, _ = w.Write([]byte("payload"))
			})).ServeHTTP(response, request)
			if response.Code != http.StatusNotAcceptable || response.Body.Len() != 0 {
				t.Fatalf("response = %d/%q, want bodyless 406", response.Code, response.Body.String())
			}
			if response.Header().Get("Vary") != "" ||
				response.Header().Get("Content-Length") != "" || response.Header().Get("Content-MD5") != "" {
				t.Fatalf("response headers = %#v, want no Vary or body-derived headers", response.Header())
			}
		})
	}
}

func TestResponsePlanCombinesBufferedTransformAndCompressionCommit(t *testing.T) {
	body := newResponseTestPlugin("body-transformer", 20, responseTestConfig{stage: "none", body: true})
	bodyBinding := checkedResponseBinding(t, "body-transformer", body, ScopeRoute, "route")
	wraps := 0
	gzipPlugin := &plan16CompressionPlugin{coding: compression.Gzip, rank: 1, wrapCall: &wraps}
	gzipPlugin.Name = "gzip"
	gzipBinding, err := BindPluginChecked(
		"gzip",
		gzipPlugin,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "gzip-route"},
	)
	if err != nil {
		t.Fatalf("BindPluginChecked(gzip) error = %v", err)
	}
	bindings := []Binding{bodyBinding, gzipBinding}
	plan, err := BuildResponsePlan(ResponsePlanInput{
		StaticBindings: bindings,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	lifecycle.SetFinalRequest(request)
	recorder := httptest.NewRecorder()
	plan.Install(NewRequestPipeline(bindings, nil), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		_, _ = w.Write([]byte("ok"))
	})).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" || wraps != 1 {
		t.Fatalf("combined response = %d/%q wraps=%d, want 200/ok/1", recorder.Code, recorder.Body.String(), wraps)
	}
}

func (p *plan16ProtocolPlugin) ResponseCapability() ResponseCapability {
	return ResponseCapability{StreamingResponseOwner: true, ExclusiveProtocol: ProtocolAI}
}

func (p *plan16ProtocolPlugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	_ http.Handler,
) (base.ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error) {
	if p.disposition == 0 {
		return 0, r, apisixctx.ResponseSourceUnknown, errors.New("missing disposition")
	}
	apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
	if p.disposition == base.ProtocolResponded {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("owned"))
	}
	return p.disposition, r, apisixctx.ResponseSourceAPISIX, nil
}

func TestBuildResponsePlanSeparatesStreamingAndConditionalOwnership(t *testing.T) {
	stream := &plan16StreamingPlugin{}
	stream.Name = "stream"
	stream.Priority = 10
	plan, err := BuildResponsePlan(ResponsePlanInput{StaticBindings: []Binding{
		resolvedPlan16Binding(t, "proxy-buffering", stream, "stream-route"),
	}})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	if len(plan.StreamingBindings()) != 1 || len(plan.BufferedBindings()) != 0 {
		t.Fatalf("plan streaming=%d buffered=%d", len(plan.StreamingBindings()), len(plan.BufferedBindings()))
	}
	if static := plan.StaticBindings(); len(static) != 1 || !static[0].Descriptor.resolved ||
		static[0].InstanceKey == (InstanceKey{}) {
		t.Fatalf("static resolved binding was not frozen into the plan: %#v", static)
	}
	bindings := plan.StreamingBindings()
	bindings[0].Provenance.ID = "mutated"
	if plan.StreamingBindings()[0].Provenance.ID != "stream-route" {
		t.Fatal("response plan leaked mutable binding storage")
	}
}

func TestPlan16CompressionUsesSharedStateAndOneFrozenWinner(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0.8, br;q=1")
	request, state := compression.Register(request,
		compression.Offer{Coding: compression.Gzip, Rank: 1},
		compression.Offer{Coding: compression.Brotli, Rank: 2},
	)
	meta := compression.ResponseMeta{
		Method: request.Method,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/plain"}},
	}
	first, second := state.Decide(meta), state.Decide(meta)
	if first != second || first.Coding != compression.Brotli || first.NotAcceptable {
		t.Fatalf("compression decisions = %#v/%#v, want one br winner", first, second)
	}
}

func TestBuildResponsePlanRejectsTwoExclusiveProtocolsWithProvenance(t *testing.T) {
	first := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	first.Name = "first"
	second := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	second.Name = "second"
	_, err := BuildResponsePlan(ResponsePlanInput{RouteTerminals: []RouteTerminalCandidate{
		{
			Identity: "grpc-web", Protocol: ProtocolGRPCWeb, Scope: ScopeRoute,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r1"}, Terminal: first,
		},
		{
			Identity: "kafka-proxy", Protocol: ProtocolKafka, Scope: ScopeRoute,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r2"}, Terminal: second,
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "grpc-web") || !strings.Contains(err.Error(), "kafka-proxy") ||
		!strings.Contains(err.Error(), "r1") || !strings.Contains(err.Error(), "r2") {
		t.Fatalf("BuildResponsePlan() error = %v, want both protocol identities and provenance", err)
	}
}

func TestStreamingExecutorRequiresSourceBeforeFirstProtocolWrite(t *testing.T) {
	terminal := &plan16LateSourceProtocolPlugin{}
	terminal.Name = "late-source"
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	executor, err = executor.WithRouteTerminals([]RouteTerminalCandidate{{
		Identity: "ai-proxy", Protocol: ProtocolAI, Scope: ScopeRoute,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "late-source"}, Terminal: terminal,
	}})
	if err != nil {
		t.Fatalf("WithRouteTerminals() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	lifecycle.SetFinalRequest(request)
	recorder := httptest.NewRecorder()
	executor.Then(nil).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "too-late") {
		t.Fatalf("late-source response = %d/%q, want stable precommit 500", recorder.Code, recorder.Body.String())
	}
}

func TestStreamingExecutorRejectsDynamicConsumerResponseOwner(t *testing.T) {
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	plugin := &plan16StreamingPlugin{}
	plugin.Name = "consumer-streaming"
	binding := Binding{
		Plugin: plugin,
		Scope:  ScopeConsumer,

		Provenance: ResourceProvenance{
			Kind: ResourceConsumer,
			ID:   "consumer-1",
		},
		Descriptor: Descriptor{Factory: "proxy-buffering"},
	}
	_, err = executor.PostResolutionHook(
		httptest.NewRequest(http.MethodGet, "/", nil),
		EffectiveBindingSet{merged: []Binding{binding}},
	)
	if err == nil || !strings.Contains(err.Error(), "proxy-buffering") ||
		!strings.Contains(err.Error(), "consumer-1") {
		t.Fatalf("PostResolutionHook() error = %v, want dynamic consumer provenance", err)
	}
}

func TestStreamingExecutorRejectsDynamicConsumerBodyFilter(t *testing.T) {
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	body := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	binding := checkedResponseBinding(t, "body-transformer", body, ScopeConsumer, "consumer-body")
	binding.Provenance.Kind = ResourceConsumer

	_, err = executor.PostResolutionHook(
		httptest.NewRequest(http.MethodGet, "/", nil),
		EffectiveBindingSet{merged: []Binding{binding}},
	)
	if err == nil || !strings.Contains(err.Error(), "body-transformer") ||
		!strings.Contains(err.Error(), "consumer-body") {
		t.Fatalf("PostResolutionHook() error = %v, want dynamic body-filter rejection", err)
	}
}

func TestStreamingExecutorAllowsDynamicConsumerDualModeBodyFilter(t *testing.T) {
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	body := newDualModeResponseTestPlugin(base.RequestResponseModeBounded)
	binding := checkedResponseBinding(t, "ai-rate-limiting", body, ScopeConsumer, "consumer-rate")
	binding.Provenance.Kind = ResourceConsumer

	replacement, err := executor.PostResolutionHook(
		httptest.NewRequest(http.MethodPost, "/ai", nil),
		EffectiveBindingSet{merged: []Binding{binding}},
	)
	if err != nil {
		t.Fatalf("PostResolutionHook() error = %v", err)
	}
	dynamic := dynamicStreamingBindings(replacement)
	if len(dynamic) != 1 || dynamic[0].Plugin != body {
		t.Fatalf("dynamic streaming bindings = %#v, want consumer dual-mode binding", dynamic)
	}
}

func TestDynamicStreamingBindingsPreserveBodyBindingWhenHeadersAreAddedLater(t *testing.T) {
	body := newDualModeResponseTestPlugin(base.RequestResponseModeStreaming)
	bodyBinding := checkedResponseBinding(t, "ai-rate-limiting", body, ScopeConsumer, "consumer-rate")
	bodyBinding.Provenance.Kind = ResourceConsumer
	header := newExecutorCORSPlugin(t, corsplugin.Config{AllowOrigins: "*"})
	headerBinding := pipelineBinding("cors", header, ScopeConsumer, 4000)
	headerBinding.Provenance = ResourceProvenance{Kind: ResourceConsumer, ID: "consumer-cors"}

	request := withDynamicStreamingBindings(
		httptest.NewRequest(http.MethodGet, "/", nil),
		[]Binding{bodyBinding},
	)
	request = withDynamicStreamingBindings(request, []Binding{headerBinding})
	dynamic := dynamicStreamingBindings(request)
	if len(dynamic) != 2 || dynamic[0].Plugin != body || dynamic[1].Plugin != header {
		t.Fatalf("dynamic bindings = %#v, want body binding followed by header binding", dynamic)
	}
}

func TestStreamingExecutorPostResolutionHookChecksBothBindingPartitions(t *testing.T) {
	executor, err := NewStreamingResponseExecutor(nil)
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	newCORSBinding := func(name string, scope Scope, kind ResourceKind) Binding {
		binding := pipelineBinding(
			"cors",
			newExecutorCORSPlugin(t, corsplugin.Config{AllowOrigins: "*"}),
			scope,
			1,
		)
		binding.Provenance = ResourceProvenance{Kind: kind, ID: name}
		return binding
	}
	global := newCORSBinding("global", ScopeGlobal, ResourceGlobalRule)
	route := newCORSBinding("route", ScopeRoute, ResourceRoute)
	consumer := newCORSBinding("consumer", ScopeConsumer, ResourceConsumer)
	group := newCORSBinding("group", ScopeConsumer, ResourceConsumerGroup)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	replacement, err := executor.PostResolutionHook(request, EffectiveBindingSet{
		global: []Binding{global},
		merged: []Binding{route, consumer, group},
	})
	if err != nil {
		t.Fatalf("PostResolutionHook() error = %v", err)
	}
	dynamic := dynamicStreamingBindings(replacement)
	if len(dynamic) != 2 || dynamic[0].Plugin != consumer.Plugin || dynamic[1].Plugin != group.Plugin {
		t.Fatalf("dynamic header bindings = %#v, want consumer and group bindings", dynamic)
	}

	body := newResponseTestPlugin(
		"body-transformer",
		1,
		responseTestConfig{stage: "none", body: true},
	)
	bodyBinding := checkedResponseBinding(t, "body-transformer", body, ScopeConsumer, "group-body")
	bodyBinding.Provenance.Kind = ResourceConsumerGroup
	_, err = executor.PostResolutionHook(request, EffectiveBindingSet{
		global: []Binding{global},
		merged: []Binding{route, bodyBinding},
	})
	if err == nil || !strings.Contains(err.Error(), "body-transformer") ||
		!strings.Contains(err.Error(), "group-body") {
		t.Fatalf("PostResolutionHook() error = %v, want dynamic body-filter provenance", err)
	}
}

func TestStreamingExecutorAppliesDynamicConsumerHeaderFilterPerRequest(t *testing.T) {
	for _, provenanceKind := range []ResourceKind{ResourceConsumer, ResourceConsumerGroup} {
		t.Run(string(provenanceKind), func(t *testing.T) {
			routeCORS := newExecutorCORSPlugin(t, corsplugin.Config{
				AllowOrigins: "*",
			})
			consumerCORS := newExecutorCORSPlugin(t, corsplugin.Config{
				AllowOrigins: "https://consumer.example",
			})
			routeBinding := pipelineBinding("cors", routeCORS, ScopeRoute, 4000)
			consumerBinding := pipelineBinding("cors", consumerCORS, ScopeConsumer, 4000)
			consumerBinding.Provenance = ResourceProvenance{
				Kind: provenanceKind,
				ID:   "dynamic-" + string(provenanceKind),
			}
			streaming, err := NewStreamingResponseExecutor([]Binding{routeBinding})
			if err != nil {
				t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
			}
			auth := newExecutorRequestPlugin(
				"auth",
				1,
				func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
					return base.ContinueRequest(r)
				},
			)
			resolverCalls := 0
			terminalCalls := 0
			pipeline := NewRequestPipeline([]Binding{
				routeBinding,
				pipelineBinding("jwt-auth", auth, ScopeRoute, 1),
			}, func(r *http.Request) (ConsumerResolution, error) {
				resolverCalls++
				if r.Header.Get("X-Use-Consumer-CORS") == "yes" {
					return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{consumerBinding}}, nil
				}
				return ConsumerResolution{Request: r, Resolved: true}, nil
			}).WithStreamingResponseExecutor(streaming)

			serve := func(origin, useConsumer string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
				request.Header.Set("Origin", origin)
				if useConsumer != "" {
					request.Header.Set("X-Use-Consumer-CORS", useConsumer)
				}
				request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
				response := httptest.NewRecorder()
				pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					terminalCalls++
					if useConsumer == "yes" {
						dynamic := dynamicStreamingBindings(request)
						if len(dynamic) != 1 {
							t.Errorf("terminal dynamic streaming bindings = %d, want 1", len(dynamic))
						}
						merged := mergeStreamingBindings(streaming.bindings, dynamic)
						if len(merged) != 1 || merged[0].Plugin != consumerCORS {
							t.Errorf("terminal merged streaming bindings = %#v, want consumer CORS", merged)
						}
					}
					w.WriteHeader(http.StatusNoContent)
					if useConsumer == "yes" {
						if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://consumer.example" {
							t.Errorf("committed CORS origin = %q, want consumer origin", got)
						}
					}
				})).ServeHTTP(response, request)
				return response
			}

			consumerResponse := serve("https://consumer.example", "yes")
			if got := consumerResponse.Header().Get("Access-Control-Allow-Origin"); got != "https://consumer.example" {
				t.Fatalf("consumer CORS origin = %q, want consumer origin", got)
			}
			if got := countExecutorVaryToken(consumerResponse.Header(), "Origin"); got != 1 {
				t.Fatalf(
					"consumer Vary: Origin count = %d, want 1 (headers=%v)",
					got,
					consumerResponse.Header().Values("Vary"),
				)
			}

			routeResponse := serve("https://route.example", "")
			if got := routeResponse.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Fatalf("route CORS origin after consumer request = %q, want wildcard route origin", got)
			}
			if got := countExecutorVaryToken(routeResponse.Header(), "Origin"); got != 0 {
				t.Fatalf(
					"route Vary: Origin count = %d, want 0 for wildcard origin (headers=%v)",
					got,
					routeResponse.Header().Values("Vary"),
				)
			}
			if resolverCalls != 2 || terminalCalls != 2 {
				t.Fatalf("resolver/terminal calls = %d/%d, want 2/2", resolverCalls, terminalCalls)
			}
		})
	}
}

func TestStreamingExecutorCORSOverridesConflictingUpstreamHeader(t *testing.T) {
	cors := newExecutorCORSPlugin(t, corsplugin.Config{
		AllowOrigins: "https://client.example",
	})
	binding := pipelineBinding("cors", cors, ScopeRoute, 4000)
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://example.com/resource", nil)
	request.Header.Set("Origin", "https://client.example")
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Access-Control-Allow-Origin", "https://upstream.example")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)

	values := response.Header().Values("Access-Control-Allow-Origin")
	if len(values) != 1 || values[0] != "https://client.example" {
		t.Fatalf(
			"Access-Control-Allow-Origin values = %v, want exactly [https://client.example]",
			values,
		)
	}
}

func TestBuildResponsePlanAcceptsRouteOwnedTerminalCandidate(t *testing.T) {
	prep := &plan16BarePlugin{}
	prep.Name = "dubbo-prep"
	terminal := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	terminal.Name = "dubbo-owner"
	plan, err := BuildResponsePlan(ResponsePlanInput{
		StaticBindings: []Binding{
			resolvedPlan16Binding(t, "dubbo-proxy", prep, "dubbo-route"),
		},
		RouteTerminals: []RouteTerminalCandidate{{
			Identity: "dubbo-proxy", Scope: ScopeRoute, Priority: 1,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "dubbo-route"},
			Protocol:   ProtocolDubbo, Terminal: terminal,
		}},
	})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	if len(plan.RouteTerminals()) != 1 || plan.RouteTerminals()[0].Terminal != terminal {
		t.Fatalf("route terminals = %#v, want supplied route owner", plan.RouteTerminals())
	}
	if static := plan.StaticBindings(); len(static) != 1 || !static[0].Descriptor.resolved ||
		static[0].InstanceKey == (InstanceKey{}) {
		t.Fatalf("static resolved binding was not frozen into the plan: %#v", static)
	}
}

func TestBuildResponsePlanRejectsSameIdentityDifferentTerminalProvenance(t *testing.T) {
	first := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	second := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	_, err := BuildResponsePlan(ResponsePlanInput{RouteTerminals: []RouteTerminalCandidate{
		{
			Identity:   "kafka-proxy",
			Protocol:   ProtocolKafka,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
			Terminal:   first,
		},
		{
			Identity:   "kafka-proxy",
			Protocol:   ProtocolKafka,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r2"},
			Terminal:   second,
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "kafka-proxy") || !strings.Contains(err.Error(), "r1") ||
		!strings.Contains(err.Error(), "r2") {
		t.Fatalf("BuildResponsePlan() error = %v, want same-identity provenance conflict", err)
	}
}

func TestStreamingExecutorExclusiveProtocolRespondsExactlyOnce(t *testing.T) {
	owner := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	owner.Name = "owner"
	binding := resolvedPlan16Binding(t, "ai-proxy", owner, "protocol-route")
	executor, err := NewStreamingResponseExecutor([]Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(httptest.NewRequest(http.MethodGet, "/", nil), time.Now())
	response := httptest.NewRecorder()
	nextCalls := 0
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ })
	executor.Then(next).ServeHTTP(response, request)
	if nextCalls != 0 || response.Code != http.StatusAccepted || response.Body.String() != "owned" {
		t.Fatalf("protocol response = next:%d status:%d body:%q", nextCalls, response.Code, response.Body.String())
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceAPISIX {
		t.Fatalf("protocol response source = %q, want apisix", lifecycle.ResponseSource())
	}
}
