package plugin

import (
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
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type responseTestConfig struct {
	stage  string
	header bool
	body   bool
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
	bufferedCalls atomic.Int32
	streamCalls   atomic.Int32
}

func newDualModeResponseTestPlugin(mode base.RequestResponseMode) *dualModeResponseTestPlugin {
	p := &dualModeResponseTestPlugin{mode: mode}
	p.Name = "dual-mode-response"
	p.SetPriority(1)
	return p
}

func (*dualModeResponseTestPlugin) Init() error     { return nil }
func (*dualModeResponseTestPlugin) PostInit() error { return nil }
func (*dualModeResponseTestPlugin) Config() any {
	return responseModeTestConfig{modes: base.ResponseModeBounded | base.ResponseModeStreaming}
}
func (*dualModeResponseTestPlugin) Handler(next http.Handler) http.Handler { return next }
func (p *dualModeResponseTestPlugin) SelectResponseMode(*http.Request) base.RequestResponseMode {
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
		RequestStage: c.stage,
		Header:       c.header,
		BufferedBody: c.body,
	}, nil
}

type responseTestPlugin struct {
	base.BasePlugin
	config   any
	request  func(http.ResponseWriter, *http.Request) base.RequestPhaseResult
	header   func(*http.Request, *base.ResponseState) error
	body     func(*http.Request, *base.ResponseState) error
	store    func(*http.Request, base.ResponseState) error
	eligible func(apisixctx.ResponseSource) bool
}

func newResponseTestPlugin(name string, priority int, config any) *responseTestPlugin {
	plugin := &responseTestPlugin{config: config}
	plugin.Name = name
	plugin.SetPriority(priority)
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
	binding := Binding{
		Plugin: owner, Scope: ScopeRoute, Stage: RequestStageAccess,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "route"}, factoryName: "ai-proxy",
	}
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
	if calls := config.calls.Load(); calls != 1 {
		t.Fatalf("DescribeBindingPhases calls = %d, want 1 per materialization", calls)
	}
}

func TestResponseRegistryHasExactDeclaredIdentities(t *testing.T) {
	responseWant := []string{
		"api-breaker", "body-transformer", "echo", "error-page", "exit-transformer",
		"graphql-proxy-cache", "proxy-cache", "response-rewrite",
		"serverless-post-function", "serverless-pre-function",
	}
	registryWant := append([]string{"ai-aliyun-content-moderation", "ai-rate-limiting"}, responseWant...)
	registryWant = append(registryWant, "grpc-transcode")
	slices.Sort(registryWant)
	got := make([]string, 0, len(responseFactoryRegistry))
	for identity := range responseFactoryRegistry {
		got = append(got, identity)
		if _, ok := requestStageRegistry[identity]; !ok {
			t.Fatalf("request-stage registry missing %q", identity)
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
		Plugin: plugin, Scope: ScopeRoute, Stage: RequestStageLegacy,
		Provenance: ResourceProvenance{Kind: ResourceRoute, ID: "r1"}, factoryName: "unknown",
	}}})
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("MaterializeResponseBindings() error = %v, want undeclared callback", err)
	}
}
