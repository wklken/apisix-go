package compiler

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
)

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
	routeID          string
	serverAddr       string
	route            resource.Route
	service          resource.Service
	runtimeAcquirer  traffic_split.RuntimeAcquirer
	upstreamResolver traffic_split.ResourceUpstreamResolver
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

func TestDefaultEffectiveBindingOpsInjectCompleteHTTPRuntimeContext(t *testing.T) {
	registry := public_api.NewRegistry()
	acquirer := testTrafficSplitRuntimeAcquirer{}
	resolver := traffic_split.ResourceUpstreamResolver(func(string) (resource.Upstream, error) {
		return resource.Upstream{}, nil
	})
	runtimeContext := effectiveBindingRuntimeContext{
		configured:        true,
		enabledFactories:  []string{"request-id", "workflow"},
		publicAPIRegistry: registry,
		serverAddr:        "127.0.0.1:9080",
		runtimeAcquirer:   acquirer,
		upstreamResolver:  resolver,
	}
	resourceContext := effectiveBindingResourceContext{
		kind:    effectiveBindingContextHTTP,
		route:   resource.Route{ID: "route-1"},
		service: resource.Service{ID: "service-1"},
	}
	instance := &runtimeContextPlugin{}
	operations := defaultEffectiveBindingOps().withDefaults([32]byte{1})
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
	if instance.registry != registry || !instance.prevalidated {
		t.Fatal("public API registry or pre-materialization validation was not injected")
	}
	if instance.routeID != "route-1" || instance.serverAddr != "127.0.0.1:9080" ||
		instance.route.ID != "route-1" || instance.service.ID != "service-1" {
		t.Fatalf("route context = %#v", instance)
	}
	if instance.runtimeAcquirer == nil || instance.upstreamResolver == nil {
		t.Fatal("traffic-split runtime context was not injected")
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
	}

	var order []string
	defaults := prepared.bindingOps.withDefaults(prepared.attempt.AttemptID())
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
			enabledFactories: []string{"request-context"},
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
	defaults := prepared.bindingOps.withDefaults(prepared.attempt.AttemptID())
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
