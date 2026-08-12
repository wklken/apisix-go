package plugin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
)

type plan16StreamingPlugin struct {
	base.BasePlugin
	closes   *int
	finishes *int
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
	binding := Binding{
		Plugin: p, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "route"}, factoryName: "proxy-buffering",
	}
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
	binding := Binding{
		Plugin: p, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "finish-route"}, factoryName: "proxy-buffering",
	}
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

type plan16CompressionPlugin struct {
	base.BasePlugin
	coding       compression.Coding
	rank         int
	eligible     func(compression.ResponseMeta) bool
	registerCall *int
	wrapCall     *int
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
	if p.wrapCall != nil {
		*p.wrapCall++
	}
	return w, nil
}

var _ CompressionOfferPlugin = (*plan16CompressionPlugin)(nil)

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
		{
			Plugin: gzipPlugin, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "gzip"}, factoryName: "gzip",
		},
		{
			Plugin: brPlugin, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "br"}, factoryName: "brotli",
		},
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
	executor, err := NewStreamingResponseExecutor([]Binding{{
		Plugin: plugin, Scope: ScopeRoute, Stage: RequestStageLegacy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "gzip-204"}, factoryName: "gzip",
	}})
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
	executor, err := NewStreamingResponseExecutor([]Binding{{
		Plugin: plugin, Scope: ScopeRoute, Stage: RequestStageLegacy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "gzip-406"}, factoryName: "gzip",
	}})
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
			if response.Header().Get("Vary") != "Accept-Encoding" ||
				response.Header().Get("Content-Length") != "" || response.Header().Get("Content-MD5") != "" {
				t.Fatalf("response headers = %#v, want Vary and no body-derived headers", response.Header())
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
	stream.SetPriority(10)
	plan, err := BuildResponsePlan(ResponsePlanInput{StaticBindings: []Binding{{
		Plugin: stream, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "stream-route"}, factoryName: "streaming-test",
	}}})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	if len(plan.StreamingBindings()) != 1 || len(plan.BufferedBindings()) != 0 {
		t.Fatalf("plan streaming=%d buffered=%d", len(plan.StreamingBindings()), len(plan.BufferedBindings()))
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
		Plugin: plugin, Scope: ScopeConsumer, Stage: RequestStageAccess,
		Provenance: ResourceProvenance{Kind: ResourceConsumer, ID: "consumer-1"}, factoryName: "proxy-buffering",
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

func TestBuildResponsePlanAcceptsRouteOwnedTerminalCandidate(t *testing.T) {
	prep := &plan16BarePlugin{}
	prep.Name = "dubbo-prep"
	terminal := &plan16ProtocolPlugin{disposition: base.ProtocolResponded}
	terminal.Name = "dubbo-owner"
	plan, err := BuildResponsePlan(ResponsePlanInput{
		StaticBindings: []Binding{{
			Plugin: prep, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
			Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "dubbo-route"}, factoryName: "dubbo-proxy",
		}},
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
	binding := Binding{
		Plugin: owner, Scope: ScopeRoute, Stage: RequestStageBeforeProxy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "protocol-route"}, factoryName: "protocol-test",
	}
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
