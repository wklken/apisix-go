package plugin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type responseTestConfig struct {
	stage           string
	header          bool
	streamingHeader bool
	body            bool
}

type responseModeTestConfig struct{ modes base.ResponseModeMask }

func (c responseModeTestConfig) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: c.modes}, nil
}

type responseOwnerTestPlugin struct {
	base.BasePlugin
	config responseModeTestConfig
}

type dualModeResponseTestPlugin struct {
	base.BasePlugin
	mode          base.RequestResponseMode
	selectMode    func(*http.Request) base.RequestResponseMode
	bufferedCalls atomic.Int32
	streamCalls   atomic.Int32
}

func newDualModeResponseTestPlugin(mode base.RequestResponseMode) *dualModeResponseTestPlugin {
	p := &dualModeResponseTestPlugin{mode: mode}
	p.Name = "dual-mode-response"
	p.Priority = 1
	return p
}

func (*dualModeResponseTestPlugin) Init() error     { return nil }
func (*dualModeResponseTestPlugin) PostInit() error { return nil }
func (*dualModeResponseTestPlugin) Config() any {
	return responseModeTestConfig{modes: base.ResponseModeBounded | base.ResponseModeStreaming}
}
func (*dualModeResponseTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *dualModeResponseTestPlugin) SelectResponseMode(r *http.Request) base.RequestResponseMode {
	if p.selectMode != nil {
		return p.selectMode(r)
	}
	return p.mode
}

func (p *dualModeResponseTestPlugin) RunBufferedBodyFilter(*http.Request, *base.ResponseState) error {
	p.bufferedCalls.Add(1)
	return nil
}

func (p *dualModeResponseTestPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	p.streamCalls.Add(1)
	return w, nil
}

func (p *responseOwnerTestPlugin) Init() error                            { return nil }
func (p *responseOwnerTestPlugin) PostInit() error                        { return nil }
func (p *responseOwnerTestPlugin) Config() any                            { return &p.config }
func (p *responseOwnerTestPlugin) Handler(next http.Handler) http.Handler { return next }

func (p *responseOwnerTestPlugin) ResponseCapability() ResponseCapability {
	return ResponseCapability{StreamingResponseOwner: true, ExclusiveProtocol: ProtocolAI}
}

type countingResponseTestConfig struct {
	descriptor base.BindingPhaseDescriptor
	calls      atomic.Int32
	fail       atomic.Bool
}

func (c *countingResponseTestConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return base.BindingPhaseDescriptor{}, errors.New("descriptor called unexpectedly")
	}
	return c.descriptor, nil
}

func (c responseTestConfig) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return base.BindingPhaseDescriptor{
		RequestStage:    c.stage,
		Header:          c.header,
		StreamingHeader: c.streamingHeader,
		BufferedBody:    c.body,
	}, nil
}

type responseTestPlugin struct {
	base.BasePlugin
	config          any
	request         func(http.ResponseWriter, *http.Request) base.RequestPhaseResult
	streamingHeader func(*http.Request, *base.StreamingResponseState) error
	header          func(*http.Request, *base.ResponseState) error
	body            func(*http.Request, *base.ResponseState) error
	store           func(*http.Request, base.ResponseState) error
	eligible        func(apisixctx.ResponseSource) bool
}

func newResponseTestPlugin(name string, priority int, config any) *responseTestPlugin {
	plugin := &responseTestPlugin{config: config}
	plugin.Name = name
	plugin.Priority = priority
	return plugin
}

func (p *responseTestPlugin) Init() error                            { return nil }
func (p *responseTestPlugin) PostInit() error                        { return nil }
func (p *responseTestPlugin) Config() any                            { return p.config }
func (p *responseTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *responseTestPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	if p.request == nil {
		return base.ContinueRequest(r)
	}
	return p.request(w, r)
}

func (p *responseTestPlugin) RunStreamingHeaderFilter(
	r *http.Request,
	state *base.StreamingResponseState,
) error {
	if p.streamingHeader != nil {
		return p.streamingHeader(r, state)
	}
	return nil
}

func (p *responseTestPlugin) RunHeaderFilter(r *http.Request, state *base.ResponseState) error {
	if p.header == nil {
		return nil
	}
	return p.header(r, state)
}

func (p *responseTestPlugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if p.body == nil {
		return nil
	}
	return p.body(r, state)
}

func (p *responseTestPlugin) RunFinalResponseStore(r *http.Request, state base.ResponseState) error {
	if p.store == nil {
		return nil
	}
	return p.store(r, state)
}

func (p *responseTestPlugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	if p.eligible == nil {
		return source == apisixctx.ResponseSourceUpstream
	}
	return p.eligible(source)
}

func checkedResponseBinding(
	t *testing.T,
	factory string,
	plugin Plugin,
	scope Scope,
	id string,
) Binding {
	t.Helper()
	binding, err := BindPluginChecked(factory, plugin, scope, ResourceProvenance{Kind: ResourceRoute, ID: id})
	if err != nil {
		t.Fatalf("BindPluginChecked(%q) error = %v", factory, err)
	}
	return binding
}

func initializedCSRFResponsePlugin(t *testing.T) Plugin {
	t.Helper()
	instance := New("csrf", base.Dependencies{
		Config:         &config.EffectiveConfig{},
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if instance == nil {
		t.Fatal("csrf plugin is not registered")
	}
	if err := instance.Init(); err != nil {
		t.Fatalf("csrf Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"key": "secret"}, instance.Config()); err != nil {
		t.Fatalf("parse csrf config: %v", err)
	}
	harness := newCompositeSecretHarness(t, 73, "csrf-response-capability")
	defer harness.close()
	harness.scope.Plugin = "csrf"
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), harness.scope, harness.capability, instance,
	); err != nil {
		t.Fatalf("materialize csrf secrets: %v", err)
	}
	if err := instance.PostInit(); err != nil {
		t.Fatalf("csrf PostInit() error = %v", err)
	}
	return instance
}

func TestCSRFResponsePlanStreamsLargeUpstreamAndBuffersEarlyStop(t *testing.T) {
	for _, scope := range []Scope{ScopeRoute, ScopeConsumer} {
		t.Run(map[Scope]string{ScopeRoute: "route", ScopeConsumer: "consumer"}[scope], func(t *testing.T) {
			instance := initializedCSRFResponsePlugin(t)
			binding := checkedResponseBinding(t, "csrf", instance, scope, "csrf-binding")
			var (
				plan     ResponsePlan
				pipeline RequestPipeline
				err      error
			)
			if scope == ScopeRoute {
				plan, err = BuildResponsePlan(ResponsePlanInput{StaticBindings: []Binding{binding}})
				pipeline = NewRequestPipeline([]Binding{binding}, nil)
			} else {
				binding.Provenance.Kind = ResourceConsumer
				plan, err = BuildResponsePlan(ResponsePlanInput{})
				pipeline = NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
					return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{binding}}, nil
				})
			}
			if err != nil {
				t.Fatalf("BuildResponsePlan() error = %v", err)
			}
			terminalCalls := 0
			payload := strings.Repeat("x", int(base.DefaultBufferedResponseMaxBytes)+1)
			handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				terminalCalls++
				apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
				_, _ = w.Write([]byte(payload))
			}))

			allowed, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "http://gateway.test/large", nil), time.Unix(0, 0),
			)
			allowedResponse := httptest.NewRecorder()
			handler.ServeHTTP(allowedResponse, allowed)
			if allowedResponse.Code != http.StatusOK || allowedResponse.Body.String() != payload {
				t.Fatalf(
					"allowed response = %d/%d bytes, want 200/%d bytes",
					allowedResponse.Code,
					allowedResponse.Body.Len(),
					len(payload),
				)
			}
			if cookies := allowedResponse.Result().Cookies(); len(cookies) != 1 {
				t.Fatalf("allowed Set-Cookie count = %d, want 1", len(cookies))
			}

			denied, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodPost, "http://gateway.test/protected", nil), time.Unix(0, 0),
			)
			deniedResponse := httptest.NewRecorder()
			handler.ServeHTTP(deniedResponse, denied)
			if deniedResponse.Code != http.StatusUnauthorized {
				t.Fatalf("denied status = %d, want 401", deniedResponse.Code)
			}
			if terminalCalls != 1 {
				t.Fatalf("terminal calls = %d, want 1", terminalCalls)
			}
			if cookies := deniedResponse.Result().Cookies(); len(cookies) != 1 {
				t.Fatalf("denied Set-Cookie count = %d, want 1", len(cookies))
			}
		})
	}
}

func TestMaterializeResponseBindingsUsesExactManifestAndPartitionOrder(t *testing.T) {
	global := newResponseTestPlugin("global", 100, responseTestConfig{stage: "none", header: true})
	merged := newResponseTestPlugin("merged", 200, responseTestConfig{stage: "none", body: true})
	plan, err := MaterializeResponseBindings(EffectiveBindingSet{
		global: []Binding{checkedResponseBinding(t, "echo", global, ScopeGlobal, "global")},
		merged: []Binding{checkedResponseBinding(t, "echo", merged, ScopeRoute, "route")},
	})
	if err != nil {
		t.Fatalf("MaterializeResponseBindings() error = %v", err)
	}
	if len(plan) != 2 || plan[0].Plugin != global || plan[0].Phases != ResponsePhaseHeader ||
		plan[1].Plugin != merged || plan[1].Phases != ResponsePhaseBufferedBody {
		t.Fatalf("materialized plan = %#v", plan)
	}
}

func TestResponseModeDescriptorCannotInventUndeclaredCallbacksOrRemoveProtocolOwner(t *testing.T) {
	owner := &responseOwnerTestPlugin{config: responseModeTestConfig{
		modes: base.ResponseModeBounded | base.ResponseModeStreaming,
	}}
	owner.Name = "response-owner"
	binding := checkedResponseBinding(t, "ai-proxy", owner, ScopeRoute, "route")
	capability, err := responseCapabilityForBinding(binding)
	if err != nil {
		t.Fatalf("responseCapabilityForBinding() error = %v", err)
	}
	if capability.StreamingBodyFilter {
		t.Fatal("response mode descriptor invented an undeclared streaming body callback")
	}
	if !capability.StreamingResponseOwner || capability.ExclusiveProtocol != ProtocolAI {
		t.Fatalf("protocol owner capability = %#v, want AI streaming owner", capability)
	}
}

func TestResponsePlanInstallAdmitsDynamicConsumerDualModeBinding(t *testing.T) {
	body := newDualModeResponseTestPlugin(base.RequestResponseModeBounded)
	binding := checkedResponseBinding(t, "ai-rate-limiting", body, ScopeConsumer, "consumer-rate")
	binding.Provenance.Kind = ResourceConsumer
	plan, err := BuildResponsePlan(ResponsePlanInput{})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{binding}}, nil
	})
	terminalCalls := 0
	handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		terminalCalls++
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		_, _ = w.Write([]byte("body"))
	}))
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/ai", nil),
		time.Now(),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "body" || terminalCalls != 1 {
		t.Fatalf(
			"response = %d/%q, terminal calls %d, want 200/body and one terminal call",
			response.Code,
			response.Body.String(),
			terminalCalls,
		)
	}
	if body.bufferedCalls.Load() != 1 || body.streamCalls.Load() != 0 {
		t.Fatalf(
			"response callbacks = buffered:%d streaming:%d, want 1/0",
			body.bufferedCalls.Load(),
			body.streamCalls.Load(),
		)
	}
}

func TestResponsePlanInstallStreamsDynamicConsumerDualModeBinding(t *testing.T) {
	body := newDualModeResponseTestPlugin(base.RequestResponseModeStreaming)
	binding := checkedResponseBinding(t, "ai-rate-limiting", body, ScopeConsumer, "consumer-rate")
	binding.Provenance.Kind = ResourceConsumer
	plan, err := BuildResponsePlan(ResponsePlanInput{})
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	pipeline := NewRequestPipeline(nil, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{binding}}, nil
	})
	handler := plan.Install(pipeline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		_, _ = w.Write([]byte("stream"))
	}))
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodPost, "/ai", nil),
		time.Now(),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "stream" {
		t.Fatalf("response = %d/%q, want 200/stream", response.Code, response.Body.String())
	}
	if body.bufferedCalls.Load() != 0 || body.streamCalls.Load() != 1 {
		t.Fatalf(
			"response callbacks = buffered:%d streaming:%d, want 0/1",
			body.bufferedCalls.Load(),
			body.streamCalls.Load(),
		)
	}
}

func TestBuildResponsePlanSelectsExactlyOneDualModeResponsePath(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mode          base.RequestResponseMode
		wantBuffered  int32
		wantStreaming int32
	}{
		{name: "bounded", mode: base.RequestResponseModeBounded, wantBuffered: 1},
		{name: "streaming", mode: base.RequestResponseModeStreaming, wantStreaming: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := newDualModeResponseTestPlugin(tc.mode)
			binding := checkedResponseBinding(t, "ai-rate-limiting", phase, ScopeRoute, tc.name)
			plan, err := BuildResponsePlan(ResponsePlanInput{StaticBindings: []Binding{binding}})
			if err != nil {
				t.Fatalf("BuildResponsePlan() error = %v", err)
			}
			handler := plan.Install(NewRequestPipeline([]Binding{binding}, nil), http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
					_, _ = w.Write([]byte("response"))
					if tc.mode == base.RequestResponseModeStreaming {
						w.(http.Flusher).Flush()
					}
				},
			))
			request, _ := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil), time.Unix(0, 0),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != "response" {
				t.Fatalf("response = %d/%q, want 200/response", response.Code, response.Body.String())
			}
			if got := phase.bufferedCalls.Load(); got != tc.wantBuffered {
				t.Fatalf("buffered calls = %d, want %d", got, tc.wantBuffered)
			}
			if got := phase.streamCalls.Load(); got != tc.wantStreaming {
				t.Fatalf("streaming calls = %d, want %d", got, tc.wantStreaming)
			}
		})
	}
}

func TestMaterializeResponseBindingsUsesPrivateFactoryIdentity(t *testing.T) {
	config := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{
		RequestStage: "none",
		Header:       true,
	}}
	plugin := newResponseTestPlugin("not-echo", 1, config)
	binding := checkedResponseBinding(t, "echo", plugin, ScopeRoute, "route")
	config.calls.Store(0)
	plan, err := MaterializeResponseBindings(EffectiveBindingSet{merged: []Binding{binding}})
	if err != nil {
		t.Fatalf("MaterializeResponseBindings() error = %v", err)
	}
	if len(plan) != 1 || plan[0].factoryKey != "echo" || plan[0].Plugin.GetName() != "not-echo" {
		t.Fatalf("plan = %#v", plan)
	}
	if calls := config.calls.Load(); calls != 0 {
		t.Fatalf("DescribeBindingPhases calls = %d, want immutable descriptor reuse", calls)
	}
}

func TestResponsePlanRequestLocalBindingsNeverResolveDescriptors(t *testing.T) {
	plan, err := BuildResponsePlan(ResponsePlanInput{})
	if err != nil {
		t.Fatal(err)
	}

	resolvedConfig := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{
		RequestStage: "none",
		Header:       true,
	}}
	resolvedPlugin := newResponseTestPlugin("echo", 1, resolvedConfig)
	resolved := checkedResponseBinding(t, "echo", resolvedPlugin, ScopeConsumer, "resolved-consumer")
	resolved.Provenance.Kind = ResourceConsumer
	if calls := resolvedConfig.calls.Load(); calls != 1 {
		t.Fatalf("resolved DescribeBindingPhases calls = %d, want 1 at construction", calls)
	}
	for range 2 {
		if _, err := plan.PostResolutionHook(
			httptest.NewRequest(http.MethodGet, "/", nil),
			EffectiveBindingSet{merged: []Binding{resolved}},
		); err != nil {
			t.Fatalf("resolved PostResolutionHook() error = %v", err)
		}
	}
	if calls := resolvedConfig.calls.Load(); calls != 1 {
		t.Fatalf("resolved DescribeBindingPhases calls = %d, want immutable 1", calls)
	}

	unresolvedConfig := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{
		RequestStage: "none",
		Header:       true,
	}}
	unresolved := Binding{
		Plugin:     newResponseTestPlugin("echo", 1, unresolvedConfig),
		Descriptor: Descriptor{Factory: "echo"},
		Scope:      ScopeConsumer,
		Provenance: ResourceProvenance{Kind: ResourceConsumer, ID: "unresolved-consumer"},
	}
	for range 2 {
		if _, err := plan.PostResolutionHook(
			httptest.NewRequest(http.MethodGet, "/", nil),
			EffectiveBindingSet{merged: []Binding{unresolved}},
		); err == nil {
			t.Fatal("unresolved PostResolutionHook() error = nil, want fail closed")
		}
	}
	if calls := unresolvedConfig.calls.Load(); calls != 0 {
		t.Fatalf("unresolved DescribeBindingPhases calls = %d, want 0", calls)
	}
}

func TestResponseRewriteSelectsExactlyOneConfiguredResponseOwner(t *testing.T) {
	tests := []struct {
		name              string
		config            responseTestConfig
		wantBuffered      int
		wantStreaming     int
		wantMetadataOwner ResponseOwnerKind
		conditional       bool
	}{
		{
			name:              "streaming header",
			config:            responseTestConfig{stage: "none", streamingHeader: true},
			wantStreaming:     1,
			wantMetadataOwner: ResponseOwnerStreamingHeaderFilter,
		},
		{
			name:              "buffered body",
			config:            responseTestConfig{stage: "none", body: true},
			wantBuffered:      1,
			wantMetadataOwner: ResponseOwnerBufferedBodyFilter,
		},
		{
			name:              "conditional early response bounded fallback",
			config:            responseTestConfig{stage: "none", streamingHeader: true},
			wantBuffered:      1,
			wantMetadataOwner: ResponseOwnerStreamingHeaderFilter,
			conditional:       true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := newResponseTestPlugin("response-rewrite", 1, test.config)
			binding := checkedResponseBinding(t, "response-rewrite", instance, ScopeRoute, "route-1")
			bindings := []Binding{binding}
			if test.conditional {
				auth := newExecutorRequestPlugin(
					"key-auth",
					100,
					func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
						return base.StopRequest(r)
					},
				)
				bindings = append(bindings, bindPluginForTest(
					"key-auth",
					auth,
					ScopeRoute,
					ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
				))
			}
			plan, err := BuildResponsePlan(bindings)
			if err != nil {
				t.Fatalf("BuildResponsePlan() error = %v", err)
			}
			if got := len(plan.BufferedBindings()); got != test.wantBuffered {
				t.Fatalf("buffered bindings = %d, want %d", got, test.wantBuffered)
			}
			if got := len(plan.StreamingBindings()); got != test.wantStreaming {
				t.Fatalf("streaming bindings = %d, want %d", got, test.wantStreaming)
			}
			phases := binding.Descriptor.response
			if !slices.Equal(phases.Owners, []ResponseOwnerKind{test.wantMetadataOwner}) {
				t.Fatalf("metadata owners = %v, want [%v]", phases.Owners, test.wantMetadataOwner)
			}
		})
	}
}

func TestDynamicResponseRewriteConditionalFallbackRunsExactlyOnce(t *testing.T) {
	staticRewrite := newResponseTestPlugin(
		"response-rewrite",
		1,
		responseTestConfig{stage: "none", streamingHeader: true},
	)
	consumerRewrite := newResponseTestPlugin(
		"response-rewrite",
		1,
		responseTestConfig{stage: "none", streamingHeader: true},
	)
	consumerRewrite.body = func(_ *http.Request, state *base.ResponseState) error {
		state.Header.Add("Set-Cookie", "consumer=1")
		return nil
	}
	consumerRewrite.streamingHeader = func(_ *http.Request, state *base.StreamingResponseState) error {
		state.Header.Add("Set-Cookie", "consumer=1")
		return nil
	}
	auth := newExecutorRequestPlugin(
		"key-auth",
		100,
		func(_ http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
			return base.ContinueRequest(r)
		},
	)
	static := []Binding{
		checkedResponseBinding(t, "response-rewrite", staticRewrite, ScopeRoute, "route-1"),
		bindPluginForTest(
			"key-auth",
			auth,
			ScopeRoute,
			ResourceProvenance{Kind: ResourceRoute, ID: "route-1"},
		),
	}
	consumer := checkedResponseBinding(t, "response-rewrite", consumerRewrite, ScopeConsumer, "consumer-1")
	consumer.Provenance.Kind = ResourceConsumer
	plan, err := BuildResponsePlan(static)
	if err != nil {
		t.Fatalf("BuildResponsePlan() error = %v", err)
	}
	pipeline := NewRequestPipeline(static, func(r *http.Request) (ConsumerResolution, error) {
		return ConsumerResolution{Request: r, Resolved: true, Bindings: []Binding{consumer}}, nil
	})
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	request, _ := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/", nil),
		time.Now(),
	)
	plan.Install(pipeline, terminal).ServeHTTP(response, request)
	if got := response.Header().Values("Set-Cookie"); !reflect.DeepEqual(got, []string{"consumer=1"}) {
		t.Fatalf("Set-Cookie = %v, want exactly one consumer value", got)
	}
}

func TestResponseRegistryHasExactDeclaredIdentities(t *testing.T) {
	responseWant := []string{
		"api-breaker", "body-transformer", "csrf", "echo", "error-page", "exit-transformer",
		"graphql-proxy-cache", "proxy-cache", "response-rewrite",
		"serverless-post-function", "serverless-pre-function",
	}
	registryWant := append([]string{"ai-aliyun-content-moderation", "ai-rate-limiting"}, responseWant...)
	registryWant = append(registryWant, "grpc-transcode")
	slices.Sort(registryWant)
	got := make([]string, 0, len(responseFactoryRegistry))
	for identity := range responseFactoryRegistry {
		got = append(got, identity)
		if _, err := DescriptorForFactory(identity); err != nil {
			t.Fatalf("registry descriptor missing %q: %v", identity, err)
		}
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, registryWant) {
		t.Fatalf("response registry = %v, want %v", got, registryWant)
	}
}

func TestMaterializeResponseBindingsRejectsUndeclaredCallback(t *testing.T) {
	plugin := newResponseTestPlugin("unknown", 1, nil)
	_, err := MaterializeResponseBindings(EffectiveBindingSet{merged: []Binding{{
		Plugin: plugin, Scope: ScopeRoute,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r1"}, Descriptor: Descriptor{Factory: "unknown"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("MaterializeResponseBindings() error = %v, want undeclared callback", err)
	}
}
