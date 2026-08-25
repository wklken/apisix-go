package compiler

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

type preparedHTTPConsumer struct {
	id       string
	consumer resource.Consumer
	bindings []plugin.Binding
}

type preparedHTTPRoute struct {
	planned   routepkg.PlannedRoute
	upstream  routepkg.UpstreamPlan
	cluster   *proxy.Cluster
	system    []plugin.Binding
	global    []plugin.Binding
	local     []plugin.Binding
	consumers []preparedHTTPConsumer
	handler   http.Handler
	hosts     []string
}

type preparedHTTPRoutes struct {
	routes      []preparedHTTPRoute
	notFound    []plugin.Binding
	quarantined []generation.ResourceKey
}

type preparedServiceBindingKey struct {
	service generation.ResourceKey
	factory string
}

func (prepared *PreparedGeneration) prepareHTTPRoutes(
	ctx context.Context,
	plan *httpPreparationPlan,
) (*preparedHTTPRoutes, error) {
	if prepared == nil || ctx == nil || plan == nil || plan.plugins == nil || plan.publicAPIRegistry == nil ||
		prepared.cleanup == nil || prepared.effective == nil || prepared.consumers == nil {
		return nil, fmt.Errorf("%w: HTTP route preparation owner is incomplete", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &preparedHTTPRoutes{
		quarantined: slices.Clone(plan.plugins.Quarantined),
	}
	serviceBindings := make(map[preparedServiceBindingKey]plugin.Binding)
	for _, planned := range plan.plugins.Routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		routeKey := generation.ResourceKey{Kind: "routes", ID: planned.Route.ID}
		checkpoint, err := prepared.cleanup.Checkpoint()
		if err != nil {
			return nil, err
		}
		registryCheckpoint := plan.publicAPIRegistry.Checkpoint()
		compiled, additions, routeErr := prepared.prepareOneHTTPRoute(
			ctx, plan, planned, routeKey, serviceBindings,
		)
		if routeErr == nil {
			result.routes = append(result.routes, compiled)
			maps.Copy(serviceBindings, additions)
			continue
		}
		rollbackErr := prepared.cleanup.Rollback(context.WithoutCancel(ctx), checkpoint)
		plan.publicAPIRegistry.Rollback(registryCheckpoint)
		if rollbackErr != nil {
			return nil, fmt.Errorf("prepare HTTP route %q: %v; rollback: %w", planned.Route.ID, routeErr, rollbackErr)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result.quarantined = append(result.quarantined, routeKey)
	}
	notFoundRuntime := effectiveBindingRuntimeContext{
		configured: true, enabledFactories: slices.Clone(plan.enabledFactories),
		publicAPIRegistry: plan.publicAPIRegistry,
		serverAddr:        httpPreparationServerAddr(prepared),
		proxyCacheZones:   slices.Clone(prepared.effective.Config.Apisix.ProxyCache.Zones),
	}
	var err error
	result.notFound, err = prepared.materializeHTTPPlansByOwner(
		ctx,
		append(slices.Clone(plan.plugins.System), plan.plugins.Global...),
		effectiveBindingResourceContext{},
		notFoundRuntime,
		false,
		generation.ResourceKey{},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (prepared *PreparedGeneration) prepareOneHTTPRoute(
	ctx context.Context,
	plan *httpPreparationPlan,
	planned routepkg.PlannedRoute,
	routeKey generation.ResourceKey,
	sharedServiceBindings map[preparedServiceBindingKey]plugin.Binding,
) (preparedHTTPRoute, map[preparedServiceBindingKey]plugin.Binding, error) {
	upstream, err := routepkg.PlanRouteUpstream(
		planned.Route,
		planned.Service,
		plan.resources.upstreams,
		plan.resources.ssls,
		&prepared.effective.Config,
	)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	var cluster *proxy.Cluster
	if upstream.ClusterConfig != nil {
		cluster, err = prepared.acquireHTTPCluster(ctx, *upstream.ClusterConfig)
		if err != nil {
			return preparedHTTPRoute{}, nil, err
		}
	}
	runtimeContext, err := prepared.httpRuntimeContextForRoute(ctx, planned.Route, plan)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	resourceContext := effectiveBindingResourceContext{
		kind: effectiveBindingContextHTTP, route: planned.Route, service: planned.Service,
	}
	system, err := prepared.materializeHTTPPlansByOwner(
		ctx, planned.System, resourceContext, runtimeContext, true, routeKey,
	)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	global, err := prepared.materializeHTTPPluginPlans(
		ctx, routeKey, plan.plugins.Global, resourceContext, runtimeContext, true,
	)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	local, err := prepared.materializeHTTPPluginPlans(
		ctx, routeKey, planned.Local, resourceContext, runtimeContext, true,
	)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	serviceBindings, additions, err := prepared.materializeHTTPServicePlans(
		ctx, routeKey, planned, resourceContext, runtimeContext, sharedServiceBindings,
	)
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	local = append(local, serviceBindings...)
	consumers := make([]preparedHTTPConsumer, 0, len(plan.plugins.Consumers))
	consumerRecords := make(map[string]routepkg.PreparedConsumerRecord, len(plan.plugins.Consumers))
	for _, id := range slices.Sorted(maps.Keys(plan.plugins.Consumers)) {
		consumer, exists := prepared.consumers.ConsumerByID(id)
		if !exists {
			return preparedHTTPRoute{}, nil, fmt.Errorf("prepared HTTP consumer %q is missing", id)
		}
		bindings, err := prepared.materializeHTTPPluginPlans(
			ctx, routeKey, plan.plugins.Consumers[id], resourceContext, runtimeContext, true,
		)
		if err != nil {
			return preparedHTTPRoute{}, nil, err
		}
		consumers = append(consumers, preparedHTTPConsumer{
			id: id, consumer: consumer, bindings: bindings,
		})
		if consumer.Username == "" {
			return preparedHTTPRoute{}, nil, fmt.Errorf("prepared HTTP consumer %q has no username", id)
		}
		if _, duplicate := consumerRecords[consumer.Username]; duplicate {
			return preparedHTTPRoute{}, nil, fmt.Errorf(
				"prepared HTTP consumer username %q is duplicated",
				consumer.Username,
			)
		}
		consumerRecords[consumer.Username] = routepkg.PreparedConsumerRecord{
			Consumer: consumer, Bindings: bindings,
		}
	}
	staticBindings := make([]plugin.Binding, 0, len(system)+len(global)+len(local))
	staticBindings = append(staticBindings, system...)
	staticBindings = append(staticBindings, global...)
	staticBindings = append(staticBindings, local...)
	runtime := routepkg.PreparedUpstreamRuntime{}
	if cluster != nil {
		runtime.LoadBalancer = cluster.LoadBalancer()
		runtime.RoundTripper = cluster.RoundTripper()
	}
	handler, err := routepkg.BuildPreparedHandler(routepkg.PreparedHandlerInput{
		Route: planned.Route, Service: planned.Service,
		StaticBindings: staticBindings, Consumers: consumerRecords,
		Upstream: upstream, Runtime: runtime,
		StaticConfig: prepared.effective.Config, SSLs: plan.resources.ssls,
	})
	if err != nil {
		return preparedHTTPRoute{}, nil, err
	}
	hosts := planned.Route.EffectiveHosts()
	if !planned.Route.HostConfigured() && !planned.Route.HostsConfigured() && planned.Route.ServiceID != "" {
		hosts = slices.Clone(planned.Service.Hosts)
	}
	return preparedHTTPRoute{
		planned: planned, upstream: upstream, cluster: cluster,
		system: system, global: global, local: local, consumers: consumers,
		handler: handler, hosts: hosts,
	}, additions, nil
}

func (prepared *PreparedGeneration) materializeHTTPServicePlans(
	ctx context.Context,
	routeKey generation.ResourceKey,
	planned routepkg.PlannedRoute,
	resourceContext effectiveBindingResourceContext,
	runtimeContext effectiveBindingRuntimeContext,
	shared map[preparedServiceBindingKey]plugin.Binding,
) ([]plugin.Binding, map[preparedServiceBindingKey]plugin.Binding, error) {
	result := make([]plugin.Binding, 0, len(planned.ServicePlans))
	additions := make(map[preparedServiceBindingKey]plugin.Binding)
	for _, plan := range planned.ServicePlans {
		if plan.Factory != "kafka-logger" || planned.Service.ID == "" {
			bindings, err := prepared.materializeHTTPPluginPlans(
				ctx, routeKey, []routepkg.PluginPlan{plan}, resourceContext, runtimeContext, true,
			)
			if err != nil {
				return nil, nil, err
			}
			result = append(result, bindings...)
			continue
		}
		key := preparedServiceBindingKey{
			service: generation.ResourceKey{Kind: "services", ID: planned.Service.ID},
			factory: plan.Factory,
		}
		if binding, exists := shared[key]; exists {
			result = append(result, binding)
			continue
		}
		if binding, exists := additions[key]; exists {
			result = append(result, binding)
			continue
		}
		bindings, err := prepared.materializeHTTPPluginPlans(
			ctx, routeKey, []routepkg.PluginPlan{plan}, resourceContext, runtimeContext, true,
		)
		if err != nil {
			return nil, nil, err
		}
		if len(bindings) != 1 {
			return nil, nil, fmt.Errorf("%w: shared service binding count is invalid", ErrInvalidInput)
		}
		additions[key] = bindings[0]
		result = append(result, bindings[0])
	}
	return result, additions, nil
}

func (prepared *PreparedGeneration) materializeHTTPPlansByOwner(
	ctx context.Context,
	plans []routepkg.PluginPlan,
	resourceContext effectiveBindingResourceContext,
	runtimeContext effectiveBindingRuntimeContext,
	recoverable bool,
	fallback generation.ResourceKey,
) ([]plugin.Binding, error) {
	result := make([]plugin.Binding, 0, len(plans))
	for _, plan := range plans {
		owner := fallback
		if plan.Scope == plugin.ScopeSystem || owner.Kind == "" {
			owner = plan.Source
		}
		bindings, err := prepared.materializeHTTPPluginPlans(
			ctx, owner, []routepkg.PluginPlan{plan}, resourceContext, runtimeContext, recoverable,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, bindings...)
	}
	return result, nil
}
