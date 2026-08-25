package route

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

// PluginPlan is one side-effect-free request to materialize a final plugin winner.
type PluginPlan struct {
	Factory        string
	Config         resource.PluginConfig
	Scope          plugin.Scope
	Provenance     plugin.ResourceProvenance
	Source         generation.ResourceKey
	FilterIdentity any
	ErrorResponse  any
	Priority       *int

	metadata pluginMetadata
}

// Apply installs the already-compiled metadata wrapper and priority on one materialized binding.
func (plan PluginPlan) Apply(binding plugin.Binding) (plugin.Binding, error) {
	if binding.Plugin == nil {
		return plugin.Binding{}, fmt.Errorf("apply plugin plan %q: binding plugin is required", plan.Factory)
	}
	if binding.Descriptor.Factory != plan.Factory || binding.Scope != plan.Scope ||
		binding.Provenance != plan.Provenance {
		return plugin.Binding{}, fmt.Errorf("apply plugin plan %q: binding authority does not match plan", plan.Factory)
	}
	wrapper, err := newMetadataPluginWithDescriptor(plan.Factory, binding.Plugin, plan.metadata, binding.Descriptor)
	if err != nil {
		return plugin.Binding{}, fmt.Errorf("apply plugin plan %q: %w", plan.Factory, err)
	}
	binding.Plugin = wrapper
	binding.Scope = plan.Scope
	binding.Provenance = plan.Provenance
	if plan.Priority != nil {
		binding.Priority = *plan.Priority
	}
	return binding, nil
}

type PlannedRoute struct {
	Route        resource.Route
	Service      resource.Service
	Local        []PluginPlan
	ServicePlans []PluginPlan
	System       []PluginPlan
}

type PlanningInput struct {
	Routes         []resource.Route
	Services       map[string]resource.Service
	PluginConfigs  map[string]resource.PluginConfigRule
	GlobalRules    []resource.GlobalRule
	Consumers      map[string]resource.Consumer
	ConsumerGroups map[string]resource.ConsumerGroup
	EnabledPlugins []string
	DynamicPlugins []string
}

type HTTPPluginPlan struct {
	Routes      []PlannedRoute
	System      []PluginPlan
	Global      []PluginPlan
	Consumers   map[string][]PluginPlan
	Quarantined []generation.ResourceKey
}

func PlanHTTPPlugins(ctx context.Context, input PlanningInput) (*HTTPPluginPlan, error) {
	if ctx == nil {
		return nil, fmt.Errorf("plan HTTP plugins: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	enabledNames := input.EnabledPlugins
	if input.DynamicPlugins != nil {
		enabledNames = input.DynamicPlugins
	}
	enabled := plugin.NewEnabledSet(enabledNames)
	result := &HTTPPluginPlan{Consumers: make(map[string][]PluginPlan)}
	systemSources := materializedPluginSources(
		buildSystemPluginConfigs(resource.Route{}, resource.Service{}),
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem},
	)
	for index := range systemSources {
		systemSources[index].scope = plugin.ScopeSystem
		systemSources[index].provenance.ID = systemSources[index].name
	}
	var err error
	result.System, err = planPluginSources(systemSources, enabled, true)
	if err != nil {
		return nil, fmt.Errorf("plan system plugins: %w", err)
	}

	globalRules := deduplicateGlobalRules(clonePlanningGlobalRules(input.GlobalRules))
	for _, rule := range globalRules {
		if rule.ID == "" {
			return nil, fmt.Errorf("plan global rule: id is required")
		}
		sources := materializedPluginSources(rule.Plugins, plugin.ResourceProvenance{
			Kind: plugin.ResourceGlobalRule, ID: rule.ID,
		})
		for index := range sources {
			sources[index].scope = plugin.ScopeGlobal
		}
		plans, err := planPluginSources(sources, enabled, false)
		if err != nil {
			return nil, fmt.Errorf("plan global rule %q: %w", rule.ID, err)
		}
		result.Global = append(result.Global, plans...)
	}

	consumerIDs := slices.Sorted(maps.Keys(input.Consumers))
	for _, id := range consumerIDs {
		consumer := clonePlanningConsumer(input.Consumers[id])
		if consumer.Username == "" {
			consumer.Username = id
		}
		var group resource.ConsumerGroup
		if consumer.GroupID != "" {
			var exists bool
			group, exists = input.ConsumerGroups[consumer.GroupID]
			if !exists {
				return nil, fmt.Errorf("plan consumer %q: consumer group %q is missing", id, consumer.GroupID)
			}
			group = clonePlanningConsumerGroup(group)
		}
		plans, err := planPluginSources(consumerPluginSources(group, consumer), enabled, false)
		if err != nil {
			return nil, fmt.Errorf("plan consumer %q: %w", id, err)
		}
		result.Consumers[id] = plans
	}

	for _, supplied := range input.Routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if supplied.Disabled() {
			continue
		}
		planned, err := planRoutePlugins(supplied, input, enabled)
		if err != nil {
			result.Quarantined = append(result.Quarantined, generation.ResourceKey{Kind: "routes", ID: supplied.ID})
			continue
		}
		result.Routes = append(result.Routes, planned)
	}
	return result, nil
}

func planRoutePlugins(routeResource resource.Route, input PlanningInput, enabled plugin.EnabledSet) (PlannedRoute, error) {
	routeResource = cloneCompileRoute(routeResource)
	if routeResource.ID == "" {
		return PlannedRoute{}, fmt.Errorf("route id is required")
	}
	var service resource.Service
	if routeResource.ServiceID != "" {
		var exists bool
		service, exists = input.Services[routeResource.ServiceID]
		if !exists {
			return PlannedRoute{}, fmt.Errorf("service %q is missing", routeResource.ServiceID)
		}
		service = clonePlanningService(service)
	}
	var pluginConfigs map[string]resource.PluginConfig
	if routeResource.PluginConfigID != "" {
		rule, exists := input.PluginConfigs[routeResource.PluginConfigID]
		if !exists {
			return PlannedRoute{}, fmt.Errorf("plugin config %q is missing", routeResource.PluginConfigID)
		}
		pluginConfigs = cloneCompilePluginConfigs(rule.Plugins)
	}
	local, serviceSources, _ := selectMaterializedPluginSources(
		routeResource.Plugins, routeResource.ID,
		pluginConfigs, routeResource.PluginConfigID,
		service.Plugins, routeResource.ServiceID,
	)
	localPlans, err := planPluginSources(local, enabled, false)
	if err != nil {
		return PlannedRoute{}, err
	}
	servicePlans, err := planPluginSources(serviceSources, enabled, false)
	if err != nil {
		return PlannedRoute{}, err
	}
	systemSources := materializedPluginSources(
		buildSystemPluginConfigs(routeResource, service),
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem},
	)
	for index := range systemSources {
		systemSources[index].scope = plugin.ScopeSystem
		systemSources[index].provenance.ID = systemSources[index].name
	}
	systemPlans, err := planPluginSources(systemSources, enabled, true)
	if err != nil {
		return PlannedRoute{}, err
	}
	return PlannedRoute{Route: routeResource, Service: service, Local: localPlans, ServicePlans: servicePlans, System: systemPlans}, nil
}

func planPluginSources(sources []materializedPluginSource, enabled plugin.EnabledSet, allowRequestContext bool) ([]PluginPlan, error) {
	plans := make([]PluginPlan, 0, len(sources))
	for _, source := range sources {
		if !enabled.Contains(source.name) && !(allowRequestContext && source.name == "request-context") {
			return nil, fmt.Errorf("plugin %q from %s %q is disabled", source.name, source.provenance.Kind, source.provenance.ID)
		}
		config, metadata, err := parsePluginMetadata(cloneCompileValue(source.config))
		if err != nil {
			return nil, fmt.Errorf("plugin %q from %s %q: %w", source.name, source.provenance.Kind, source.provenance.ID, err)
		}
		if metadata.disabled {
			continue
		}
		priority := metadata.priority
		if priority != nil {
			owned := *priority
			priority = &owned
		}
		plans = append(plans, PluginPlan{
			Factory: source.name, Config: cloneCompileValue(config), Scope: source.scope,
			Provenance: source.provenance, Source: sourceGenerationKey(source.provenance),
			FilterIdentity: cloneCompileValue(metadata.identityFilter),
			ErrorResponse:  cloneCompileValue(metadata.errorResponse), Priority: priority, metadata: metadata,
		})
	}
	return plans, nil
}

func sourceGenerationKey(provenance plugin.ResourceProvenance) generation.ResourceKey {
	kinds := map[plugin.ResourceKind]string{
		plugin.ResourceRoute: "routes", plugin.ResourceService: "services",
		plugin.ResourcePluginConfig: "plugin_configs", plugin.ResourceGlobalRule: "global_rules",
		plugin.ResourceConsumer: "consumers", plugin.ResourceConsumerGroup: "consumer_groups",
		plugin.ResourceSystem: "system",
	}
	return generation.ResourceKey{Kind: kinds[provenance.Kind], ID: provenance.ID}
}

func clonePlanningService(source resource.Service) resource.Service {
	source.Plugins = cloneCompilePluginConfigs(source.Plugins)
	source.Hosts = append([]string(nil), source.Hosts...)
	source.Upstream = cloneCompileUpstream(source.Upstream)
	return source
}

func clonePlanningConsumer(source resource.Consumer) resource.Consumer {
	source.Plugins = cloneCompilePluginConfigs(source.Plugins)
	source.Labels = cloneCompileAnyMap(source.Labels)
	return source
}

func clonePlanningConsumerGroup(source resource.ConsumerGroup) resource.ConsumerGroup {
	source.Plugins = cloneCompilePluginConfigs(source.Plugins)
	return source
}

func clonePlanningGlobalRules(source []resource.GlobalRule) []resource.GlobalRule {
	result := make([]resource.GlobalRule, len(source))
	for index, rule := range source {
		result[index] = rule
		result[index].Plugins = cloneCompilePluginConfigs(rule.Plugins)
	}
	return result
}
