package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
)

type metadataResponseContractPlugin struct {
	name        string
	descriptor  base.BindingPhaseDescriptor
	headerCalls int
	bodyCalls   int
	storeCalls  int
	priority    int
}

func (p *metadataResponseContractPlugin) Init() error               { return nil }
func (p *metadataResponseContractPlugin) PostInit() error           { return nil }
func (p *metadataResponseContractPlugin) GetSchema() string         { return "" }
func (p *metadataResponseContractPlugin) GetMetadataSchema() string { return "" }
func (p *metadataResponseContractPlugin) GetPriority() int          { return p.priority }
func (p *metadataResponseContractPlugin) GetName() string           { return p.name }
func (p *metadataResponseContractPlugin) Handler(next http.Handler) http.Handler {
	return next
}
func (p *metadataResponseContractPlugin) Config() any { return p }

func (p *metadataResponseContractPlugin) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	return p.descriptor, nil
}

func (p *metadataResponseContractPlugin) RunRequestPhase(
	_ http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	return base.ContinueRequest(r)
}

func (p *metadataResponseContractPlugin) RunHeaderFilter(*http.Request, *base.ResponseState) error {
	p.headerCalls++
	return nil
}

func (p *metadataResponseContractPlugin) RunBufferedBodyFilter(
	*http.Request,
	*base.ResponseState,
) error {
	p.bodyCalls++
	return nil
}

func (p *metadataResponseContractPlugin) RunFinalResponseStore(
	*http.Request,
	base.ResponseState,
) error {
	p.storeCalls++
	return nil
}

func TestMetadataWrappersForwardOnlyRegistryDeclaredRequestAndResponseInterfaces(t *testing.T) {
	echo := &metadataResponseContractPlugin{
		name:       "echo",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", Header: true},
	}
	wrapped, err := newMetadataPlugin("echo", echo, pluginMetadata{})
	if err != nil {
		t.Fatalf("newMetadataPlugin() error = %v", err)
	}
	if wrapped != echo {
		t.Fatalf("metadata without options returned %T, want original plugin", wrapped)
	}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrapped, err = newMetadataPlugin("echo", echo, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin() error = %v", err)
	}
	if _, ok := wrapped.(base.HeaderFilterPlugin); !ok {
		t.Fatalf("wrapped echo does not expose declared header callback: %T", wrapped)
	}
	if _, ok := wrapped.(base.BufferedBodyFilterPlugin); ok {
		t.Fatalf("wrapped echo exposes undeclared body callback: %T", wrapped)
	}
	if _, ok := wrapped.(base.RequestPhasePlugin); ok {
		t.Fatalf("wrapped echo exposes undeclared request callback: %T", wrapped)
	}
	request := httptest.NewRequest(http.MethodGet, "/?enabled=no", nil)
	state := &base.ResponseState{Status: http.StatusOK}
	if err := wrapped.(base.HeaderFilterPlugin).RunHeaderFilter(request, state); err != nil {
		t.Fatalf("filtered header callback error = %v", err)
	}
	if echo.headerCalls != 0 {
		t.Fatalf("filtered header callback calls = %d, want 0", echo.headerCalls)
	}
	request = httptest.NewRequest(http.MethodGet, "/?enabled=yes", nil)
	if err := wrapped.(base.HeaderFilterPlugin).RunHeaderFilter(request, state); err != nil {
		t.Fatalf("header callback error = %v", err)
	}
	if echo.headerCalls != 1 {
		t.Fatalf("header callback calls = %d, want 1", echo.headerCalls)
	}

	cache := &metadataResponseContractPlugin{name: "proxy-cache"}
	cacheWrapped, err := newMetadataPlugin("proxy-cache", cache, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newMetadataPlugin(proxy-cache) error = %v", err)
	}
	if _, ok := cacheWrapped.(base.RequestPhasePlugin); !ok {
		t.Fatalf("wrapped proxy-cache does not expose declared request callback: %T", cacheWrapped)
	}
	if _, ok := cacheWrapped.(base.FinalResponseStorePlugin); !ok {
		t.Fatalf("wrapped proxy-cache does not expose declared store callback: %T", cacheWrapped)
	}
}

func TestMetadataResponseFilterPriorityAndErrorResponseRemainExact(t *testing.T) {
	bodyPlugin := &metadataResponseContractPlugin{
		name:       "body-transformer",
		descriptor: base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true},
		priority:   321,
	}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrapped, err := newMetadataPlugin("body-transformer", bodyPlugin, pluginMetadata{
		filter:        filter,
		errorResponse: map[string]any{"message": "denied"},
	})
	if err != nil {
		t.Fatalf("newMetadataPlugin(body-transformer) error = %v", err)
	}
	if wrapped.GetPriority() != 321 {
		t.Fatalf("priority = %d, want 321", wrapped.GetPriority())
	}
	if _, ok := wrapped.(base.BufferedBodyFilterPlugin); !ok {
		t.Fatalf("wrapped body-transformer does not expose body callback: %T", wrapped)
	}
	request := httptest.NewRequest(http.MethodGet, "/?enabled=no", nil)
	if err := wrapped.(base.BufferedBodyFilterPlugin).RunBufferedBodyFilter(
		request,
		&base.ResponseState{Status: http.StatusOK},
	); err != nil {
		t.Fatalf("filtered body callback error = %v", err)
	}
	if bodyPlugin.bodyCalls != 0 {
		t.Fatalf("filtered body callback calls = %d, want 0", bodyPlugin.bodyCalls)
	}
}

func TestStrictRouteMaterializationUsesBindPluginCheckedForStaticAndConsumer(t *testing.T) {
	builder := NewBuilder(nil)
	routeContext := builder.pluginRouteContext(resource.Route{ID: "strict-binding-route"})
	source := materializedPluginSource{
		name:       "body-transformer",
		config:     map[string]any{"request": map[string]any{"template": "ok"}},
		scope:      plugin.ScopeRoute,
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeContext.routeID},
	}
	bindings, err := builder.initPluginBindingsStrict(
		[]materializedPluginSource{source},
		routeContext,
		pluginInitOptions{},
	)
	if err != nil {
		t.Fatalf("static binding initialization error = %v", err)
	}
	if len(bindings) != 1 || bindings[0].Stage != plugin.RequestStageRewrite {
		t.Fatalf("static binding = %#v, want one rewrite-stage checked binding", bindings)
	}
	consumerSources := consumerPluginSources(
		resource.ConsumerGroup{},
		resource.Consumer{Username: "strict-consumer", Plugins: map[string]resource.PluginConfig{
			"body-transformer": source.config,
		}},
	)
	consumerBindings, err := builder.initPluginBindingsStrict(
		consumerSources,
		routeContext,
		pluginInitOptions{},
	)
	if err != nil {
		t.Fatalf("consumer binding initialization error = %v", err)
	}
	if len(consumerBindings) != 1 || consumerBindings[0].Stage != plugin.RequestStageRewrite {
		t.Fatalf("consumer binding = %#v, want one rewrite-stage checked binding", consumerBindings)
	}
}
