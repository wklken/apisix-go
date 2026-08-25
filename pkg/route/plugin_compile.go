package route

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"net"
	"net/http"
	"slices"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
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
		return plugin.Binding{}, fmt.Errorf(
			"apply plugin plan %q: binding plugin is required",
			plan.Factory,
		)
	}
	if binding.Descriptor.Factory != plan.Factory || binding.Scope != plan.Scope ||
		binding.Provenance != plan.Provenance {
		return plugin.Binding{}, fmt.Errorf(
			"apply plugin plan %q: binding authority does not match plan",
			plan.Factory,
		)
	}
	wrapper, err := newMetadataPluginWithDescriptor(
		plan.Factory,
		binding.Plugin,
		plan.metadata,
		binding.Descriptor,
	)
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
	Profiles       appconfig.ProfileSelection
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
	if err := validateSecurityGlobalRulePolicy(input.Profiles, globalRules, ""); err != nil {
		return nil, fmt.Errorf("plan global rules: %w", err)
	}
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
				return nil, fmt.Errorf(
					"plan consumer %q: consumer group %q is missing",
					id,
					consumer.GroupID,
				)
			}
			group = clonePlanningConsumerGroup(group)
		}
		plans, err := planPluginSources(consumerPluginSources(group, consumer), enabled, false)
		if err != nil {
			return nil, fmt.Errorf("plan consumer %q: %w", id, err)
		}
		result.Consumers[id] = plans
	}

	for _, supplied := range normalizeRouteOrder(input.Routes) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if supplied.Disabled() {
			continue
		}
		planned, err := planRoutePlugins(supplied, input, enabled)
		if err != nil {
			result.Quarantined = append(
				result.Quarantined,
				generation.ResourceKey{Kind: "routes", ID: supplied.ID},
			)
			continue
		}
		result.Routes = append(result.Routes, planned)
	}
	return result, nil
}

func planRoutePlugins(
	routeResource resource.Route,
	input PlanningInput,
	enabled plugin.EnabledSet,
) (PlannedRoute, error) {
	routeResource = cloneCompileRoute(routeResource)
	if routeResource.ID == "" {
		return PlannedRoute{}, fmt.Errorf("route id is required")
	}
	if err := validateRouteCompatibility(routeResource); err != nil {
		return PlannedRoute{}, err
	}
	if err := validatePlannedRouteURIs(routeResource); err != nil {
		return PlannedRoute{}, err
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
			return PlannedRoute{}, fmt.Errorf(
				"plugin config %q is missing",
				routeResource.PluginConfigID,
			)
		}
		pluginConfigs = cloneCompilePluginConfigs(rule.Plugins)
	}
	local, serviceSources, _ := selectMaterializedPluginSources(
		routeResource.Plugins, routeResource.ID,
		pluginConfigs, routeResource.PluginConfigID,
		service.Plugins, routeResource.ServiceID,
	)
	if err := validateSecurityMaterializedPluginSources(
		input.Profiles,
		append(slices.Clone(local), serviceSources...),
		routeResource.ID,
	); err != nil {
		return PlannedRoute{}, err
	}
	if err := validateSecurityGlobalRulePolicy(input.Profiles, input.GlobalRules, routeResource.ID); err != nil {
		return PlannedRoute{}, err
	}
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
	return PlannedRoute{
		Route:        routeResource,
		Service:      service,
		Local:        localPlans,
		ServicePlans: servicePlans,
		System:       systemPlans,
	}, nil
}

func validatePlannedRouteURIs(routeResource resource.Route) error {
	uris := routeResource.Uris
	if len(uris) == 0 && routeResource.Uri != "" {
		uris = []string{routeResource.Uri}
	}
	effective := make(map[string]string, len(uris))
	for _, uri := range uris {
		converted, err := convertURI(uri)
		if err != nil {
			return fmt.Errorf("route %q URI %q: %w", routeResource.ID, uri, err)
		}
		identity := effectiveRouteURI(converted)
		if previous, exists := effective[identity]; exists {
			return fmt.Errorf(
				"route %q: duplicate effective URI %q (from %q and %q)",
				routeResource.ID,
				identity,
				previous,
				uri,
			)
		}
		effective[identity] = uri
	}
	return nil
}

func planPluginSources(
	sources []materializedPluginSource,
	enabled plugin.EnabledSet,
	allowRequestContext bool,
) ([]PluginPlan, error) {
	plans := make([]PluginPlan, 0, len(sources))
	for _, source := range sources {
		if !enabled.Contains(source.name) &&
			(!allowRequestContext || source.name != "request-context") {
			return nil, fmt.Errorf(
				"plugin %q from %s %q is disabled",
				source.name,
				source.provenance.Kind,
				source.provenance.ID,
			)
		}
		config, metadata, err := parsePluginMetadata(cloneCompileValue(source.config))
		if err != nil {
			return nil, fmt.Errorf(
				"plugin %q from %s %q: %w",
				source.name,
				source.provenance.Kind,
				source.provenance.ID,
				err,
			)
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
			ErrorResponse: cloneCompileValue(
				metadata.errorResponse,
			), Priority: priority, metadata: metadata,
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

func consumerPluginSources(group resource.ConsumerGroup, consumer resource.Consumer) []materializedPluginSource {
	sourcesByName := make(map[string]materializedPluginSource, len(group.Plugins)+len(consumer.Plugins))
	for _, name := range slices.Sorted(maps.Keys(group.Plugins)) {
		if isConsumerCredentialOnly(name) {
			continue
		}
		sourcesByName[name] = materializedPluginSource{
			name:       name,
			config:     group.Plugins[name],
			scope:      plugin.ScopeConsumer,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumerGroup, ID: consumer.GroupID},
		}
	}
	for _, name := range slices.Sorted(maps.Keys(consumer.Plugins)) {
		if isConsumerCredentialOnly(name) {
			continue
		}
		sourcesByName[name] = materializedPluginSource{
			name:       name,
			config:     consumer.Plugins[name],
			scope:      plugin.ScopeConsumer,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: consumer.Username},
		}
	}

	names := slices.Sorted(maps.Keys(sourcesByName))
	sources := make([]materializedPluginSource, 0, len(names))
	for _, name := range names {
		sources = append(sources, sourcesByName[name])
	}
	return sources
}

type materializedPluginSource struct {
	name       string
	config     resource.PluginConfig
	scope      plugin.Scope
	provenance plugin.ResourceProvenance
}

func selectMaterializedPluginSources(
	routePlugins map[string]resource.PluginConfig,
	routeID string,
	pluginConfigPlugins map[string]resource.PluginConfig,
	pluginConfigID string,
	servicePlugins map[string]resource.PluginConfig,
	serviceID string,
) (localSources, serviceSources []materializedPluginSource, effective map[string]resource.PluginConfig) {
	effective = clonePluginConfigs(routePlugins)
	localSources = materializedPluginSources(
		routePlugins,
		plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeID},
	)
	for _, name := range slices.Sorted(maps.Keys(pluginConfigPlugins)) {
		if _, exists := effective[name]; exists {
			continue
		}
		config := pluginConfigPlugins[name]
		effective[name] = config
		localSources = append(localSources, materializedPluginSource{
			name:       name,
			config:     config,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourcePluginConfig, ID: pluginConfigID},
			scope:      plugin.ScopeRoute,
		})
	}

	servicePluginConfigs := make(map[string]resource.PluginConfig)
	for _, name := range slices.Sorted(maps.Keys(servicePlugins)) {
		if _, exists := effective[name]; exists {
			continue
		}
		config := servicePlugins[name]
		if name == "kafka-logger" && serviceID != "" {
			servicePluginConfigs[name] = config
			serviceSources = append(serviceSources, materializedPluginSource{
				name:       name,
				config:     config,
				provenance: plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: serviceID},
				scope:      plugin.ScopeRoute,
			})
			continue
		}
		effective[name] = config
		localSources = append(localSources, materializedPluginSource{
			name:       name,
			config:     config,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: serviceID},
			scope:      plugin.ScopeRoute,
		})
	}
	maps.Copy(effective, servicePluginConfigs)
	return localSources, serviceSources, effective
}

func materializedPluginSources(
	pluginConfigs map[string]resource.PluginConfig,
	provenance plugin.ResourceProvenance,
) []materializedPluginSource {
	names := slices.Sorted(maps.Keys(pluginConfigs))
	sources := make([]materializedPluginSource, 0, len(names))
	for _, name := range names {
		sources = append(sources, materializedPluginSource{
			name:       name,
			config:     pluginConfigs[name],
			scope:      plugin.ScopeRoute,
			provenance: provenance,
		})
	}
	return sources
}

func buildSystemPluginConfigs(
	r resource.Route,
	service resource.Service,
) map[string]resource.PluginConfig {
	return map[string]resource.PluginConfig{
		"request-context": buildRequestContextConfig(r, service),
	}
}

func matchedHost(r resource.Route) string {
	if len(r.Hosts) > 0 {
		return r.Hosts[0]
	}
	return ""
}

type pluginMetadata struct {
	disabled       bool
	priority       *int
	filter         *pluginexpr.Expression
	identityFilter any
	errorResponse  any
}

func (m pluginMetadata) instanceIdentity(config any) plugin.InstanceIdentityInput {
	return plugin.InstanceIdentityInput{
		PluginConfig:  config,
		Filter:        m.identityFilter,
		ErrorResponse: m.errorResponse,
	}
}

func parsePluginMetadata(config resource.PluginConfig) (resource.PluginConfig, pluginMetadata, error) {
	values, ok := config.(map[string]any)
	if !ok {
		return config, pluginMetadata{}, nil
	}
	rawMetadata, ok := values["_meta"]
	if !ok {
		return config, pluginMetadata{}, nil
	}
	metadataValues, ok := rawMetadata.(map[string]any)
	if !ok {
		return nil, pluginMetadata{}, fmt.Errorf("_meta must be an object")
	}

	pluginConfig := make(map[string]any, len(values)-1)
	for name, value := range values {
		if name != "_meta" {
			pluginConfig[name] = value
		}
	}
	metadata := pluginMetadata{}
	if value, ok := metadataValues["disable"]; ok {
		disabled, ok := value.(bool)
		if !ok {
			return nil, pluginMetadata{}, fmt.Errorf("_meta.disable must be a boolean")
		}
		metadata.disabled = disabled
	}
	if value, ok := metadataValues["priority"]; ok {
		priority, err := parsePluginPriority(value)
		if err != nil {
			return nil, pluginMetadata{}, err
		}
		metadata.priority = &priority
	}
	if value, ok := metadataValues["filter"]; ok {
		filter, err := pluginexpr.Compile(value)
		if err != nil {
			return nil, pluginMetadata{}, fmt.Errorf("_meta.filter: %w", err)
		}
		metadata.filter = filter
		metadata.identityFilter = value
	}
	if value, ok := metadataValues["error_response"]; ok {
		switch value.(type) {
		case string, map[string]any:
			metadata.errorResponse = value
		default:
			return nil, pluginMetadata{}, fmt.Errorf("_meta.error_response must be a string or object")
		}
	}
	return pluginConfig, metadata, nil
}

func newMetadataPluginWithDescriptor(
	factoryName string,
	p plugin.Plugin,
	metadata pluginMetadata,
	descriptor plugin.Descriptor,
) (plugin.Plugin, error) {
	wrapped, err := newMetadataRequestAndBufferedPluginWithDescriptor(factoryName, p, metadata, descriptor)
	if err != nil || wrapped == p {
		return wrapped, err
	}
	{
		_, sanitizerCallback := p.(base.LogSnapshotSanitizerPlugin)
		ownsSanitizer := descriptor.HasPhase(plugin.PhaseLog) && sanitizerCallback
		ownsLog := descriptor.HasPhase(plugin.PhaseLog)
		ownsSnapshot := descriptor.OwnsSnapshotFinalizer()
		switch {
		case ownsSanitizer:
			return metadataSnapshotSanitizerPlugin{
				Plugin: wrapped,
				target: p,
				filter: metadata.filter,
			}, nil
		case ownsLog:
			return metadataLogPlugin{Plugin: wrapped, target: p, filter: metadata.filter}, nil
		case ownsSnapshot:
			request, ok := wrapped.(base.RequestPhasePlugin)
			if !ok {
				return nil, fmt.Errorf("factory %q declares snapshot finalizer without request callback", factoryName)
			}
			return metadataRequestSnapshotPlugin{
				Plugin:  wrapped,
				request: request,
				target:  p,
				filter:  metadata.filter,
			}, nil
		}
	}
	capability := descriptor.ResponseCapability()
	if !descriptor.HasPhase(plugin.PhaseHeaderFilter) &&
		!descriptor.HasPhase(plugin.PhaseBodyFilter) &&
		!descriptor.HasPhase(plugin.PhaseProtocol) {
		return wrapped, nil
	}
	streaming := metadataStreamingPlugin{Plugin: wrapped, target: p, filter: metadata.filter}
	switch factoryName {
	case "ai-proxy", "ai-proxy-multi", "grpc-web":
		return metadataProtocolPlugin{metadataStreamingPlugin: streaming}, nil
	}
	if capability.HeaderFilter || capability.StreamingBodyFilter || capability.CompressionOffer {
		return streaming, nil
	}
	return wrapped, nil
}

func deduplicateGlobalRules(globalRules []resource.GlobalRule) []resource.GlobalRule {
	seen := make(map[string]struct{})
	duplicates := make(map[string]struct{})
	for _, rule := range globalRules {
		for name := range rule.Plugins {
			if _, ok := seen[name]; ok {
				duplicates[name] = struct{}{}
				continue
			}
			seen[name] = struct{}{}
		}
	}

	result := make([]resource.GlobalRule, len(globalRules))
	for index, rule := range globalRules {
		result[index] = rule
		if rule.Plugins == nil {
			continue
		}
		plugins := make(map[string]resource.PluginConfig, len(rule.Plugins))
		for name, config := range rule.Plugins {
			if _, duplicate := duplicates[name]; duplicate {
				continue
			}
			plugins[name] = config
		}
		result[index].Plugins = plugins
	}
	return result
}

type routeProtocolTerminals struct {
	kafka     base.ExclusiveProtocolTerminal
	dubbo     base.ExclusiveProtocolTerminal
	httpDubbo base.ExclusiveProtocolTerminal
}

type routeKafkaTerminal struct{ handler http.Handler }

func (t routeKafkaTerminal) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	_ http.Handler,
) (base.ProtocolDisposition, *http.Request, ctx.ResponseSource, error) {
	ctx.SetRequestResponseSource(r, ctx.ResponseSourceUpstream)
	hijacked := false
	tracked := httpsnoop.Wrap(w, httpsnoop.Hooks{
		Hijack: func(hijack httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				connection, readWriter, err := hijack()
				if err == nil {
					hijacked = true
				}
				return connection, readWriter, err
			}
		},
	})
	if t.handler != nil {
		t.handler.ServeHTTP(tracked, r)
	}
	if hijacked {
		return base.ProtocolHijacked, r, ctx.ResponseSourceUpstream, nil
	}
	return base.ProtocolResponded, r, ctx.ResponseSourceUpstream, nil
}

type routeDubboTerminal struct {
	lb      pxy.LoadBalancer
	targets map[string]compiledUpstreamTarget
	retries int
}

func (t routeHTTPDubboTerminal) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, ctx.ResponseSource, error) {
	ctx.SetRequestResponseSource(r, ctx.ResponseSourceUpstream)
	if (t.lb == nil && traffic_split.GetOverride(r) == nil) ||
		!serveHTTPDubboIfConfiguredCompiled(w, r, t.lb, t.targets, t.retries) {
		if next != nil {
			next.ServeHTTP(w, r)
		}
	}
	return base.ProtocolResponded, r, ctx.ResponseSourceUpstream, nil
}

func (t routeDubboTerminal) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, ctx.ResponseSource, error) {
	ctx.SetRequestResponseSource(r, ctx.ResponseSourceUpstream)
	if (t.lb == nil && traffic_split.GetOverride(r) == nil) ||
		!serveDubboIfConfiguredCompiled(w, r, t.lb, t.targets, t.retries) {
		if next != nil {
			next.ServeHTTP(w, r)
		}
	}
	return base.ProtocolResponded, r, ctx.ResponseSourceUpstream, nil
}

type routeHTTPDubboTerminal struct {
	lb      pxy.LoadBalancer
	targets map[string]compiledUpstreamTarget
	retries int
}
