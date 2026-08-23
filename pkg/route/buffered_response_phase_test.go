package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
)

func newTestMetadataPlugin(
	factoryName string,
	p plugin.Plugin,
	metadata pluginMetadata,
) (plugin.Plugin, error) {
	descriptor, err := plugin.ResolveDescriptorForFactory(factoryName, p)
	if err != nil {
		return nil, err
	}
	return newMetadataPluginWithDescriptor(factoryName, p, metadata, descriptor)
}

type metadataResponseContractPlugin struct {
	name        string
	descriptor  base.BindingPhaseDescriptor
	headerCalls int
	bodyCalls   int
	storeCalls  int
	priority    int
}

type metadataStreamingContractPlugin struct {
	metadataResponseContractPlugin
	streamingHeaderCalls int
	streamingBodyCalls   int
	protocolCalls        int
}

func (p *metadataStreamingContractPlugin) RunStreamingHeaderFilter(
	*http.Request,
	*base.StreamingResponseState,
) error {
	p.streamingHeaderCalls++
	return nil
}

func (p *metadataStreamingContractPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	_ *http.Request,
) (http.ResponseWriter, error) {
	p.streamingBodyCalls++
	return w, nil
}

func (*metadataStreamingContractPlugin) RegisterCompressionOffers(
	*http.Request,
	*compression.State,
) []compression.Offer {
	return []compression.Offer{{Coding: compression.Gzip}}
}

func (*metadataStreamingContractPlugin) WrapCompression(
	w http.ResponseWriter,
	_ *http.Request,
	_ *compression.State,
	_ compression.Decision,
) (http.ResponseWriter, error) {
	return w, nil
}

func (p *metadataStreamingContractPlugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, apisixctx.ResponseSource, error) {
	p.protocolCalls++
	if next != nil {
		next.ServeHTTP(w, r)
	}
	return base.ProtocolResponded, r, apisixctx.ResponseSourceUnknown, nil
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
	wrapped, err := newTestMetadataPlugin("echo", echo, pluginMetadata{})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin() error = %v", err)
	}
	if wrapped != echo {
		t.Fatalf("metadata without options returned %T, want original plugin", wrapped)
	}
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	wrapped, err = newTestMetadataPlugin("echo", echo, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin() error = %v", err)
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
	cacheWrapped, err := newTestMetadataPlugin("proxy-cache", cache, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin(proxy-cache) error = %v", err)
	}
	if _, ok := cacheWrapped.(base.RequestPhasePlugin); !ok {
		t.Fatalf("wrapped proxy-cache does not expose declared request callback: %T", cacheWrapped)
	}
	if _, ok := cacheWrapped.(base.FinalResponseStorePlugin); !ok {
		t.Fatalf("wrapped proxy-cache does not expose declared store callback: %T", cacheWrapped)
	}
}

func TestMetadataWrappersPreservePlan16StreamingAndProtocolCallbacks(t *testing.T) {
	filter, err := pluginexpr.Compile([]any{[]any{"arg_enabled", "==", "yes"}})
	if err != nil {
		t.Fatalf("compile filter: %v", err)
	}
	streaming := &metadataStreamingContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "gzip",
	}}
	wrapped, err := newTestMetadataPlugin("gzip", streaming, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin(gzip) error = %v", err)
	}
	header, headerOK := wrapped.(base.StreamingHeaderFilterPlugin)
	body, bodyOK := wrapped.(base.StreamingBodyFilterPlugin)
	_, compressionOK := wrapped.(plugin.CompressionOfferPlugin)
	if !headerOK || !bodyOK || !compressionOK {
		t.Fatalf(
			"wrapped gzip methods = header:%v body:%v compression:%v (%T)",
			headerOK,
			bodyOK,
			compressionOK,
			wrapped,
		)
	}
	request := httptest.NewRequest(http.MethodGet, "/?enabled=no", nil)
	if err := header.RunStreamingHeaderFilter(request, &base.StreamingResponseState{}); err != nil {
		t.Fatalf("filtered streaming header error = %v", err)
	}
	if _, err := body.WrapStreamingResponse(httptest.NewRecorder(), request); err != nil {
		t.Fatalf("filtered streaming body error = %v", err)
	}
	if streaming.streamingHeaderCalls != 0 || streaming.streamingBodyCalls != 0 {
		t.Fatalf("filtered streaming calls = %d/%d", streaming.streamingHeaderCalls, streaming.streamingBodyCalls)
	}

	protocol := &metadataStreamingContractPlugin{metadataResponseContractPlugin: metadataResponseContractPlugin{
		name: "ai-proxy",
	}}
	wrapped, err = newTestMetadataPlugin("ai-proxy", protocol, pluginMetadata{filter: filter})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin(ai-proxy) error = %v", err)
	}
	terminal, ok := wrapped.(base.ExclusiveProtocolTerminal)
	if !ok {
		t.Fatalf("wrapped ai-proxy does not expose protocol callback: %T", wrapped)
	}
	nextCalls := 0
	_, _, _, err = terminal.RunExclusiveProtocol(
		httptest.NewRecorder(),
		request,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ }),
	)
	if err != nil || protocol.protocolCalls != 0 || nextCalls != 1 {
		t.Fatalf("filtered protocol = err:%v plugin:%d next:%d", err, protocol.protocolCalls, nextCalls)
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
	wrapped, err := newTestMetadataPlugin("body-transformer", bodyPlugin, pluginMetadata{
		filter:        filter,
		errorResponse: map[string]any{"message": "denied"},
	})
	if err != nil {
		t.Fatalf("newTestMetadataPlugin(body-transformer) error = %v", err)
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
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
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
	if len(bindings) != 1 || bindings[0].Descriptor.RequestStage() != plugin.RequestStageRewrite {
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
	if len(consumerBindings) != 1 || consumerBindings[0].Descriptor.RequestStage() != plugin.RequestStageRewrite {
		t.Fatalf("consumer binding = %#v, want one rewrite-stage checked binding", consumerBindings)
	}
}
