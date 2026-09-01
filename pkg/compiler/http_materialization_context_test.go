package compiler

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

func TestAPISIXPluginContextFromHTTPBindingPlan(t *testing.T) {
	effective := &appconfig.EffectiveConfig{Config: appconfig.Config{Deployment: appconfig.Deployment{
		Role:            "traditional",
		RoleTraditional: appconfig.RoleTraditionalConfig{ConfigProvider: "etcd"},
		Etcd:            appconfig.Etcd{Prefix: "/literal/apisix/"},
	}}}
	tests := []struct {
		name       string
		provenance plugin.ResourceProvenance
		source     generation.ResourceKey
		wantParent string
	}{
		{
			name: "route", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-1"},
			source: generation.ResourceKey{
				Kind: "routes",
				ID:   "route-1",
			}, wantParent: "/literal/apisix//routes/route-1",
		},
		{
			name: "service", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: "service-1"},
			source: generation.ResourceKey{
				Kind: "services",
				ID:   "service-1",
			}, wantParent: "/literal/apisix//services/service-1",
		},
		{
			name:       "plugin config",
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourcePluginConfig, ID: "config-1"},
			source: generation.ResourceKey{
				Kind: "plugin_configs",
				ID:   "config-1",
			},
			wantParent: "/literal/apisix//plugin_configs/config-1",
		},
		{
			name: "global rule", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-1"},
			source: generation.ResourceKey{
				Kind: "global_rules",
				ID:   "global-1",
			}, wantParent: "/literal/apisix//global_rules/global-1",
		},
		{
			name: "consumer", provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "alice"},
			source: generation.ResourceKey{
				Kind: "consumers",
				ID:   "alice",
			}, wantParent: "/literal/apisix//consumers/alice",
		},
		{
			name:       "consumer group",
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: "group-1"},
			source: generation.ResourceKey{
				Kind: "consumer_groups",
				ID:   "group-1",
			},
			wantParent: "/literal/apisix//consumer_groups/group-1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context, useDefault, err := apisixPluginContextForPlan(effective, routepkg.PluginPlan{
				Factory: "limit-count", Config: map[string]any{"count": 2},
				SourceConfig: map[string]any{"count": 2},
				Scope:        plugin.ScopeRoute, Provenance: test.provenance, Source: test.source,
			})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := context.ParentResourceKey()
			if err != nil || parent != test.wantParent || useDefault {
				t.Fatalf("context = (%#v, %v, %v), want parent %q", context, useDefault, err, test.wantParent)
			}
			context.SourceConfig["count"] = 99
			if got := context.SourceConfig["count"]; got != 99 {
				t.Fatalf("context source config mutation did not take effect: %v", got)
			}
		})
	}

	standalone := *effective
	standalone.Config.Deployment.Role = "data_plane"
	standalone.Config.Deployment.RoleDataPlane.ConfigProvider = "yaml"
	context, _, err := apisixPluginContextForPlan(&standalone, routepkg.PluginPlan{
		Factory: "limit-count", Config: map[string]any{}, SourceConfig: map[string]any{},
		Scope:      plugin.ScopeRoute,
		Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "route-1"},
		Source:     generation.ResourceKey{Kind: "routes", ID: "route-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if parent, err := context.ParentResourceKey(); err != nil || parent != "/routes/route-1" {
		t.Fatalf("standalone parent = (%q, %v), want /routes/route-1", parent, err)
	}
}

func TestAPISIXPluginContextConsumerOverride(t *testing.T) {
	planned := routepkg.PlannedRoute{
		Local:        []routepkg.PluginPlan{{Factory: "limit-count"}},
		ServicePlans: []routepkg.PluginPlan{{Factory: "kafka-logger"}},
	}
	source := []routepkg.PluginPlan{
		{Factory: "limit-count", SourceConfig: map[string]any{"count": 1}},
		{Factory: "kafka-logger", SourceConfig: map[string]any{}},
		{Factory: "request-id", SourceConfig: map[string]any{}},
	}
	marked := markConsumerPluginOverrides(source, planned)
	if !marked[0].ConsumerOverride || !marked[1].ConsumerOverride || marked[2].ConsumerOverride {
		t.Fatalf("consumer override marks = %#v", marked)
	}
	if source[0].ConsumerOverride || source[1].ConsumerOverride || source[2].ConsumerOverride {
		t.Fatal("consumer override marking mutated the shared consumer plan")
	}
	if marked[0].SourceConfig["_skip_rewrite_in_consumer"] != true ||
		marked[1].SourceConfig["_skip_rewrite_in_consumer"] != true ||
		marked[2].SourceConfig["_from_consumer"] != true {
		t.Fatalf("consumer APISIX identity markers = %#v", marked)
	}
	if _, exists := source[0].SourceConfig["_skip_rewrite_in_consumer"]; exists {
		t.Fatal("consumer identity marking mutated the shared source document")
	}
}

func TestAPISIXPluginContextUsesProviderVersions(t *testing.T) {
	resourceWithOrigin := func(kind, id, key, modified string) generation.Resource {
		return generation.Resource{
			Key: generation.ResourceKey{Kind: kind, ID: id},
			Origin: generation.ResourceOrigin{
				Provider: "etcd/v1/cluster", ResourceKey: key, ModifiedIndex: modified,
			},
			Value: []byte(`{}`),
		}
	}
	snapshot, err := generation.NewSnapshotWithSource(1, []generation.Resource{
		resourceWithOrigin("routes", "r1", "/apisix/routes/r1", "11"),
		resourceWithOrigin("plugin_configs", "pc1", "/apisix/plugin_configs/pc1", "13"),
		resourceWithOrigin("services", "s1", "/apisix/services/s1", "17"),
		resourceWithOrigin("consumers", "alice", "/apisix/consumers/alice", "19"),
		resourceWithOrigin("consumer_groups", "g1", "/apisix/consumer_groups/g1", "23"),
		resourceWithOrigin("global_rules", "global", "/apisix/global_rules/global", "29"),
	}, nil, map[string]string{"consumers": "5", "global_rules": "7"})
	if err != nil {
		t.Fatal(err)
	}
	effective := &appconfig.EffectiveConfig{Config: appconfig.Config{Deployment: appconfig.Deployment{
		Role: "traditional", RoleTraditional: appconfig.RoleTraditionalConfig{ConfigProvider: "etcd"},
	}}}
	resourceContext := effectiveBindingResourceContext{
		kind:    effectiveBindingContextHTTP,
		route:   resource.Route{ID: "r1", PluginConfigID: "pc1", ServiceID: "s1"},
		service: resource.Service{ID: "s1"},
	}

	consumer, _, err := apisixPluginContextForPreparedPlan(effective, snapshot, routepkg.PluginPlan{
		Factory: "limit-conn", SourceConfig: map[string]any{}, Scope: plugin.ScopeConsumer,
		Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "alice"},
		Source:     generation.ResourceKey{Kind: "consumers", ID: "alice"}, ConsumerGroupID: "g1",
	}, resourceContext)
	if err != nil {
		t.Fatal(err)
	}
	if parent, _ := consumer.ParentResourceKey(); parent != "/apisix/consumers/alice" ||
		consumer.ConfigType != "route&service&consumer&consumer_group" ||
		consumer.ConfigVersion != "11#13&17&5&23" {
		t.Fatalf("consumer APISIX context = %#v, parent=%q", consumer, parent)
	}

	global, _, err := apisixPluginContextForPreparedPlan(effective, snapshot, routepkg.PluginPlan{
		Factory: "limit-conn", SourceConfig: map[string]any{}, Scope: plugin.ScopeGlobal,
		Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global"},
		Source:     generation.ResourceKey{Kind: "global_rules", ID: "global"},
	}, resourceContext)
	if err != nil {
		t.Fatal(err)
	}
	if global.ConfigType != "global_rule" || global.ConfigVersion != "7" {
		t.Fatalf("global APISIX context = %#v", global)
	}
}

type testTrafficSplitRuntimeAcquirer struct{}

func (testTrafficSplitRuntimeAcquirer) Acquire(
	*traffic_split.Upstream,
	map[string]int,
	map[string]int,
) (*traffic_split.Runtime, error) {
	return &traffic_split.Runtime{}, nil
}

type runtimeContextPlugin struct {
	enabled          func(string) bool
	registry         *public_api.Registry
	purgeRegistry    *graphql_proxy_cache.Registry
	routeID          string
	serverAddr       string
	route            resource.Route
	service          resource.Service
	runtimeAcquirer  traffic_split.RuntimeAcquirer
	upstreamResolver traffic_split.ResourceUpstreamResolver
	protoResolver    grpc_transcode.ProtoResolver
	configuredZones  []appconfig.Zone
	zonesSet         bool
	prevalidated     bool
}

func (*runtimeContextPlugin) Init() error                            { return nil }
func (*runtimeContextPlugin) PostInit() error                        { return nil }
func (*runtimeContextPlugin) Handler(next http.Handler) http.Handler { return next }
func (*runtimeContextPlugin) Config() any                            { return &struct{}{} }
func (*runtimeContextPlugin) GetSchema() string                      { return `{}` }
func (*runtimeContextPlugin) GetMetadataSchema() string              { return "" }
func (*runtimeContextPlugin) GetPriority() int                       { return 0 }
func (*runtimeContextPlugin) GetName() string                        { return "runtime-context-test" }

func (p *runtimeContextPlugin) SetPluginEnabledChecker(checker func(string) bool) {
	p.enabled = checker
}

func (p *runtimeContextPlugin) SetPublicAPIRegistry(registry *public_api.Registry) {
	p.registry = registry
}

func (p *runtimeContextPlugin) SetPurgeRegistry(registry *graphql_proxy_cache.Registry) {
	p.purgeRegistry = registry
}

func (p *runtimeContextPlugin) SetConfiguredZones(zones []appconfig.Zone) {
	p.configuredZones = zones
	p.zonesSet = true
}

func (p *runtimeContextPlugin) ValidatePreMaterialization() error {
	p.prevalidated = true
	return nil
}

func (p *runtimeContextPlugin) SetRouteContext(routeID string, serverAddr string) {
	p.routeID, p.serverAddr = routeID, serverAddr
}

func (p *runtimeContextPlugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.route, p.service = route, service
}

func (p *runtimeContextPlugin) SetRuntimeAcquirer(acquirer traffic_split.RuntimeAcquirer) {
	p.runtimeAcquirer = acquirer
}

func (p *runtimeContextPlugin) SetUpstreamResolver(resolver traffic_split.ResourceUpstreamResolver) {
	p.upstreamResolver = resolver
}

func (p *runtimeContextPlugin) SetProtoResolver(resolver grpc_transcode.ProtoResolver) {
	p.protoResolver = resolver
}

func TestDefaultEffectiveBindingOpsInjectCompleteHTTPRuntimeContext(t *testing.T) {
	registry := public_api.NewRegistry()
	purgeRegistry := graphql_proxy_cache.NewRegistry()
	acquirer := testTrafficSplitRuntimeAcquirer{}
	resolver := traffic_split.ResourceUpstreamResolver(func(string) (resource.Upstream, error) {
		return resource.Upstream{}, nil
	})
	protoResolver := grpc_transcode.ProtoResolver(func(string) (string, error) { return "proto", nil })
	runtimeContext := effectiveBindingRuntimeContext{
		configured:        true,
		enabledFactories:  []string{"request-id", "workflow"},
		publicAPIRegistry: registry,
		purgeRegistry:     purgeRegistry,
		serverAddr:        "127.0.0.1:9080",
		runtimeAcquirer:   acquirer,
		upstreamResolver:  resolver,
		protoResolver:     protoResolver,
	}
	resourceContext := effectiveBindingResourceContext{
		kind:    effectiveBindingContextHTTP,
		route:   resource.Route{ID: "route-1"},
		service: resource.Service{ID: "service-1"},
	}
	instance := &runtimeContextPlugin{}
	operations := defaultEffectiveBindingOps().withDefaults(1)
	operations.applyBootstrap(instance, runtimeContext)
	if err := operations.preMaterialize(instance); err != nil {
		t.Fatal(err)
	}
	operations.applyRouteContext(instance, runtimeContext, resourceContext)
	operations.applyContext(instance, resourceContext)
	operations.applyTrafficRuntime(instance, runtimeContext)

	if instance.enabled == nil || !instance.enabled("workflow") || instance.enabled("disabled") {
		t.Fatal("enabled-plugin checker was not generation-scoped")
	}
	if instance.purgeRegistry != purgeRegistry {
		t.Fatal("generation-local GraphQL purge registry was not injected")
	}
	if instance.registry != registry || !instance.prevalidated {
		t.Fatal("public API registry or pre-materialization validation was not injected")
	}
	if instance.routeID != "route-1" || instance.serverAddr != "127.0.0.1:9080" ||
		instance.route.ID != "route-1" || instance.service.ID != "service-1" {
		t.Fatalf("route context = %#v", instance)
	}
	if instance.runtimeAcquirer == nil || instance.upstreamResolver == nil || instance.protoResolver == nil {
		t.Fatal("HTTP plugin runtime context was not injected")
	}
}

func TestDefaultEffectiveBindingOpsInjectConfiguredZones(t *testing.T) {
	tests := []struct {
		name  string
		zones []appconfig.Zone
	}{
		{name: "explicit empty snapshot"},
		{name: "configured snapshot", zones: []appconfig.Zone{{Name: "cache-one", MemorySize: "1M"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &runtimeContextPlugin{}
			defaultEffectiveBindingOps().withDefaults(1).applyBootstrap(
				instance,
				effectiveBindingRuntimeContext{configured: true, proxyCacheZones: test.zones},
			)

			if !instance.zonesSet {
				t.Fatal("generation-local proxy-cache zones were not injected")
			}
			if !reflect.DeepEqual(instance.configuredZones, test.zones) {
				t.Fatalf("configured zones = %#v, want %#v", instance.configuredZones, test.zones)
			}
			if len(test.zones) > 0 {
				test.zones[0].Name = "mutated"
				if instance.configuredZones[0].Name != "cache-one" {
					t.Fatal("injected proxy-cache zones alias the runtime context")
				}
			}
		})
	}
}

func TestEffectiveBindingMaterializerAppliesHTTPRuntimeContextBeforePostInit(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	spec := fixture.pluginSpec("request-id", "route-runtime-context")
	spec.runtimeContext = effectiveBindingRuntimeContext{
		configured:        true,
		enabledFactories:  []string{"request-id"},
		publicAPIRegistry: public_api.NewRegistry(),
		serverAddr:        "127.0.0.1:9080",
		runtimeAcquirer:   testTrafficSplitRuntimeAcquirer{},
		upstreamResolver: func(string) (resource.Upstream, error) {
			return resource.Upstream{}, nil
		},
		protoResolver: func(string) (string, error) { return "proto", nil },
	}

	var order []string
	defaults := prepared.bindingOps.withDefaults(prepared.preparation.Generation())
	prepared.bindingOps.applyBootstrap = func(plugin.Plugin, effectiveBindingRuntimeContext) {
		order = append(order, "bootstrap")
	}
	prepared.bindingOps.preMaterialize = func(plugin.Plugin) error {
		order = append(order, "pre-materialize")
		return nil
	}
	prepared.bindingOps.applyRouteContext = func(
		plugin.Plugin,
		effectiveBindingRuntimeContext,
		effectiveBindingResourceContext,
	) {
		order = append(order, "route")
	}
	prepared.bindingOps.applyContext = func(instance plugin.Plugin, value effectiveBindingResourceContext) {
		order = append(order, "resource")
		defaults.applyContext(instance, value)
	}
	prepared.bindingOps.applyTrafficRuntime = func(plugin.Plugin, effectiveBindingRuntimeContext) {
		order = append(order, "traffic")
	}
	prepared.bindingOps.postInit = func(instance plugin.Plugin) error {
		order = append(order, "post-init")
		return defaults.postInit(instance)
	}

	if _, err := prepared.materializeEffectiveBindings(
		context.Background(),
		[]effectiveBindingSpec{spec},
	); err != nil {
		t.Fatal(err)
	}
	want := []string{"bootstrap", "pre-materialize", "route", "resource", "traffic", "post-init"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("initialization order = %v, want %v", order, want)
	}
}

func TestCloneEffectiveBindingRuntimeContextOwnsEnabledFactories(t *testing.T) {
	enabled := []string{"workflow", "request-id", "workflow"}
	registry := public_api.NewRegistry()
	resolver := traffic_split.ResourceUpstreamResolver(func(string) (resource.Upstream, error) {
		return resource.Upstream{}, nil
	})
	protoResolver := grpc_transcode.ProtoResolver(func(string) (string, error) { return "proto", nil })
	cloned, err := cloneEffectiveBindingRuntimeContext(
		generation.DomainHTTP,
		effectiveBindingResourceContext{
			kind:  effectiveBindingContextHTTP,
			route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{
			configured:        true,
			enabledFactories:  enabled,
			publicAPIRegistry: registry,
			runtimeAcquirer:   testTrafficSplitRuntimeAcquirer{},
			upstreamResolver:  resolver,
			protoResolver:     protoResolver,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	enabled[0] = "mutated"
	want := []string{"request-id", "workflow"}
	if !reflect.DeepEqual(cloned.enabledFactories, want) {
		t.Fatalf("enabled factories = %v, want %v", cloned.enabledFactories, want)
	}
	if cloned.publicAPIRegistry != registry {
		t.Fatal("generation-local public API registry identity changed")
	}
	if resolved, err := cloned.protoResolver("root.proto"); err != nil || resolved != "proto" {
		t.Fatalf("cloned proto resolver = %q, %v", resolved, err)
	}
}

func TestCloneEffectiveBindingRuntimeContextRejectsIncompleteHTTPContext(t *testing.T) {
	_, err := cloneEffectiveBindingRuntimeContext(
		generation.DomainHTTP,
		effectiveBindingResourceContext{
			kind:  effectiveBindingContextHTTP,
			route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{configured: true},
	)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("clone runtime context error = %v, want ErrInvalidInput", err)
	}
}

func TestCloneEffectiveBindingRuntimeContextAllowsGenerationWideHTTPContext(t *testing.T) {
	registry := public_api.NewRegistry()
	cloned, err := cloneEffectiveBindingRuntimeContext(
		generation.DomainHTTP,
		effectiveBindingResourceContext{kind: effectiveBindingContextNone},
		effectiveBindingRuntimeContext{
			configured: true, publicAPIRegistry: registry,
			enabledFactories: []string{"example-plugin"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cloned.configured || cloned.publicAPIRegistry != registry || cloned.runtimeAcquirer != nil ||
		cloned.upstreamResolver != nil {
		t.Fatalf("generation-wide runtime context = %#v", cloned)
	}
}

func TestRecoverableEffectiveBindingMaterializationRollsBackOnlyFailedRoute(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(
		t,
		[]string{"request-id", "response-rewrite"},
		nil,
	)
	defaults := prepared.bindingOps.withDefaults(prepared.preparation.Generation())
	prepared.bindingOps.postInit = func(instance plugin.Plugin) error {
		if instance.GetName() == "request-id" {
			return errors.New("route-scoped post-init failure")
		}
		return defaults.postInit(instance)
	}

	bindings, err := prepared.materializeEffectiveBindingsRecoverable(
		context.Background(),
		[]effectiveBindingSpec{fixture.pluginSpec("request-id", "bad-route")},
	)
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("recoverable route failure = (%#v, %v)", bindings, err)
	}
	if prepared.terminal || fixture.registry.Len() != 0 {
		t.Fatalf("recoverable route failure terminal/leases = %v/%d", prepared.terminal, fixture.registry.Len())
	}

	prepared.bindingOps.postInit = defaults.postInit
	bindings, err = prepared.materializeEffectiveBindingsRecoverable(
		context.Background(),
		[]effectiveBindingSpec{fixture.pluginSpec("response-rewrite", "good-route")},
	)
	if err != nil || len(bindings) != 1 || bindings[0].Plugin == nil {
		t.Fatalf("later route materialization = (%#v, %v), want one binding", bindings, err)
	}
}

func TestRecoverableEffectiveBindingFinalizerFailureRollsBackMaterializedBindings(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	bindings, err := prepared.materializeEffectiveBindingsRecoverableFinalized(
		context.Background(),
		[]effectiveBindingSpec{fixture.pluginSpec("request-id", "bad-finalizer")},
		func([]plugin.Binding) ([]plugin.Binding, error) {
			return nil, errors.New("metadata wrapper failure")
		},
	)
	if !errors.Is(err, errEffectiveBindingMaterializationFailed) || bindings != nil {
		t.Fatalf("finalizer failure = (%#v, %v)", bindings, err)
	}
	if prepared.terminal || fixture.registry.Len() != 0 {
		t.Fatalf("finalizer rollback terminal/leases = %v/%d", prepared.terminal, fixture.registry.Len())
	}
}
