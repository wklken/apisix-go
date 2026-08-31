package route

import (
	"bufio"
	"context"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"

	"github.com/felixge/httpsnoop"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
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
		buildSystemPluginConfigs(enabled),
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem},
	)
	for index := range systemSources {
		systemSources[index].scope = plugin.ScopeSystem
		systemSources[index].provenance.ID = systemSources[index].name
	}
	var err error
	result.System, err = planPluginSources(systemSources, enabled)
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
		plans, err := planPluginSources(sources, enabled)
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
		plans, err := planPluginSources(consumerPluginSources(group, consumer), enabled)
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
	localPlans, err := planPluginSources(local, enabled)
	if err != nil {
		return PlannedRoute{}, err
	}
	servicePlans, err := planPluginSources(serviceSources, enabled)
	if err != nil {
		return PlannedRoute{}, err
	}
	systemSources := materializedPluginSources(
		buildSystemPluginConfigs(enabled),
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem},
	)
	for index := range systemSources {
		systemSources[index].scope = plugin.ScopeSystem
		systemSources[index].provenance.ID = systemSources[index].name
	}
	systemPlans, err := planPluginSources(systemSources, enabled)
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
) ([]PluginPlan, error) {
	plans := make([]PluginPlan, 0, len(sources))
	for _, source := range sources {
		if !enabled.Contains(source.name) {
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
		if source.name == "error-log-logger" && source.scope != plugin.ScopeSystem {
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
	source.Labels = cloneCompileAnyMap(source.Labels)
	source.Hosts = append([]string(nil), source.Hosts...)
	source.Script = slices.Clone(source.Script)
	source.Upstream = cloneCompileUpstream(source.Upstream)
	return source
}

func clonePlanningConsumer(source resource.Consumer) resource.Consumer {
	source.Plugins = cloneCompilePluginConfigs(source.Plugins)
	source.Labels = cloneCompileAnyMap(source.Labels)
	source.AuthConf = cloneCompileValue(source.AuthConf)
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

func isConsumerCredentialOnly(name string) bool {
	switch name {
	case "basic-auth", "hmac-auth", "jwe-decrypt", "jwt-auth", "key-auth", "ldap-auth", "multi-auth", "wolf-rbac":
		return true
	default:
		return false
	}
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

func clonePluginConfigs(source map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	cloned := make(map[string]resource.PluginConfig, len(source))
	maps.Copy(cloned, source)
	return cloned
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

func buildSystemPluginConfigs(enabled plugin.EnabledSet) map[string]resource.PluginConfig {
	configs := make(map[string]resource.PluginConfig)
	if enabled.Contains("log-rotate") {
		configs["log-rotate"] = map[string]any{}
	}
	if enabled.Contains("error-log-logger") {
		configs["error-log-logger"] = map[string]any{}
	}
	return configs
}

type pluginMetadata struct {
	disabled       bool
	priority       *int
	filter         *pluginexpr.Expression
	identityFilter any
	errorResponse  any
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

type metadataPlugin struct {
	plugin.Plugin
	filter        *pluginexpr.Expression
	errorResponse any
}

func (p metadataPlugin) Handler(next http.Handler) http.Handler {
	var handler http.Handler
	if p.errorResponse != nil {
		handler = p.errorResponseHandler(next)
	} else {
		handler = p.Plugin.Handler(next)
	}
	if p.filter == nil {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.filter.Eval(func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}) {
			next.ServeHTTP(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

func (p metadataPlugin) errorResponseHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled := false
		errorResponseWritten := false
		responseHeaderWritten := false
		wrappedNext := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nextCalled = true
			next.ServeHTTP(w, r)
		})
		wrappedWriter := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(status int) {
					if errorResponseWritten {
						return
					}
					responseHeaderWritten = true
					if !nextCalled && status >= http.StatusBadRequest {
						errorResponseWritten = true
						writeMetadataErrorResponse(w, status, p.errorResponse)
						return
					}
					writeHeader(status)
				}
			},
			Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(body []byte) (int, error) {
					if errorResponseWritten {
						return len(body), nil
					}
					if !responseHeaderWritten {
						responseHeaderWritten = true
					}
					return write(body)
				}
			},
		})
		p.Plugin.Handler(wrappedNext).ServeHTTP(wrappedWriter, r)
	})
}

type metadataRequestPlugin struct {
	plugin.Plugin
	phase         base.RequestPhasePlugin
	filter        *pluginexpr.Expression
	errorResponse any
}

func (p metadataRequestPlugin) Handler(next http.Handler) http.Handler {
	// Keep direct callers on the wrapped plugin's original Handler path. Some
	// request-phase plugins own package-local lifecycle/release fallbacks there.
	return (metadataPlugin{
		Plugin:        p.Plugin,
		filter:        p.filter,
		errorResponse: p.errorResponse,
	}).Handler(next)
}

func (p metadataRequestPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	if p.filter != nil && !p.filter.Eval(func(name string) any {
		return pluginexpr.RequestValue(r, name)
	}) {
		return base.ContinueRequest(r)
	}
	if p.errorResponse == nil {
		return p.phase.RunRequestPhase(w, r)
	}
	return p.phase.RunRequestPhase(metadataErrorResponseWriter(w, p.errorResponse), r)
}

type metadataResponseHeaderPlugin struct {
	metadataPlugin
	header base.HeaderFilterPlugin
}

func (p metadataResponseHeaderPlugin) RunHeaderFilter(
	r *http.Request,
	state *base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.header.RunHeaderFilter(r, state)
}

func (p metadataResponseHeaderPlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

type metadataResponseBodyPlugin struct {
	metadataPlugin
	body base.BufferedBodyFilterPlugin
}

func (p metadataResponseBodyPlugin) RunBufferedBodyFilter(
	r *http.Request,
	state *base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.body.RunBufferedBodyFilter(r, state)
}

func (p metadataResponseBodyPlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

type metadataResponseHeaderBodyPlugin struct {
	metadataPlugin
	header base.HeaderFilterPlugin
	body   base.BufferedBodyFilterPlugin
}

func (p metadataResponseHeaderBodyPlugin) RunHeaderFilter(
	r *http.Request,
	state *base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.header.RunHeaderFilter(r, state)
}

func (p metadataResponseHeaderBodyPlugin) RunBufferedBodyFilter(
	r *http.Request,
	state *base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.body.RunBufferedBodyFilter(r, state)
}

func (p metadataResponseHeaderBodyPlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

type metadataResponseStorePlugin struct {
	metadataPlugin
	store base.FinalResponseStorePlugin
}

func (p metadataResponseStorePlugin) RunFinalResponseStore(
	r *http.Request,
	state base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.store.RunFinalResponseStore(r, state)
}

func (p metadataResponseStorePlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

type metadataRequestBodyPlugin struct {
	metadataRequestPlugin
	body base.BufferedBodyFilterPlugin
}

func (p metadataRequestBodyPlugin) RunBufferedBodyFilter(
	r *http.Request,
	state *base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.body.RunBufferedBodyFilter(r, state)
}

func (p metadataRequestBodyPlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

type metadataRequestStorePlugin struct {
	metadataRequestPlugin
	store base.FinalResponseStorePlugin
}

type metadataLogPlugin struct {
	plugin.Plugin
	target plugin.Plugin
	filter *pluginexpr.Expression
}

type metadataSnapshotSanitizerPlugin struct {
	plugin.Plugin
	target plugin.Plugin
	filter *pluginexpr.Expression
}

func (p metadataSnapshotSanitizerPlugin) LogCapturePolicy() base.LogCapturePolicy {
	provider, ok := p.target.(base.LogCapturePolicyPlugin)
	if !ok {
		return base.LogCapturePolicy{}
	}
	return provider.LogCapturePolicy()
}

func (p metadataSnapshotSanitizerPlugin) ShouldSanitizeLogSnapshot(snapshot base.LogSnapshot) bool {
	return metadataSnapshotFilterMatches(p.filter, snapshot)
}

func (p metadataSnapshotSanitizerPlugin) SanitizeLogSnapshot(snapshot *base.LogSnapshot) error {
	sanitizer, ok := p.target.(base.LogSnapshotSanitizerPlugin)
	if !ok {
		return fmt.Errorf("plugin %q has no log sanitizer callback", p.target.GetName())
	}
	return sanitizer.SanitizeLogSnapshot(snapshot)
}

func (p metadataLogPlugin) LogCapturePolicy() base.LogCapturePolicy {
	provider, ok := p.target.(base.LogCapturePolicyPlugin)
	if !ok {
		return base.LogCapturePolicy{}
	}
	return provider.LogCapturePolicy()
}

func (p metadataLogPlugin) RunLogPhase(snapshot base.LogSnapshot) error {
	if !metadataSnapshotFilterMatches(p.filter, snapshot) {
		return nil
	}
	phase, ok := p.target.(base.LogPhasePlugin)
	if !ok {
		return fmt.Errorf("plugin %q has no log callback", p.target.GetName())
	}
	return phase.RunLogPhase(snapshot)
}

type metadataRequestSnapshotPlugin struct {
	plugin.Plugin
	request base.RequestPhasePlugin
	target  plugin.Plugin
	filter  *pluginexpr.Expression
}

func (p metadataRequestSnapshotPlugin) RunRequestPhase(
	w http.ResponseWriter,
	r *http.Request,
) base.RequestPhaseResult {
	return p.request.RunRequestPhase(w, r)
}

func (p metadataRequestSnapshotPlugin) LogCapturePolicy() base.LogCapturePolicy {
	provider, ok := p.target.(base.LogCapturePolicyPlugin)
	if !ok {
		return base.LogCapturePolicy{}
	}
	return provider.LogCapturePolicy()
}

func (p metadataRequestSnapshotPlugin) RunSnapshotFinalizer(snapshot base.LogSnapshot) error {
	if !metadataSnapshotFilterMatches(p.filter, snapshot) {
		return nil
	}
	finalizer, ok := p.target.(base.SnapshotFinalizerPlugin)
	if !ok {
		return fmt.Errorf("plugin %q has no snapshot finalizer callback", p.target.GetName())
	}
	return finalizer.RunSnapshotFinalizer(snapshot)
}

type metadataStreamingPlugin struct {
	plugin.Plugin
	target plugin.Plugin
	filter *pluginexpr.Expression
}

func (p metadataStreamingPlugin) RunStreamingHeaderFilter(
	r *http.Request,
	state *base.StreamingResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	header, ok := p.target.(base.StreamingHeaderFilterPlugin)
	if !ok {
		return fmt.Errorf("plugin %q has no streaming header callback", p.target.GetName())
	}
	return header.RunStreamingHeaderFilter(r, state)
}

func (p metadataStreamingPlugin) WrapStreamingResponse(
	w http.ResponseWriter,
	r *http.Request,
) (http.ResponseWriter, error) {
	if !metadataFilterMatches(p.filter, r) {
		return w, nil
	}
	body, ok := p.target.(base.StreamingBodyFilterPlugin)
	if !ok {
		return nil, fmt.Errorf("plugin %q has no streaming body callback", p.target.GetName())
	}
	return body.WrapStreamingResponse(w, r)
}

func (p metadataStreamingPlugin) RegisterCompressionOffers(
	r *http.Request,
	state *compression.State,
) []compression.Offer {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	offer, ok := p.target.(plugin.CompressionOfferPlugin)
	if !ok {
		return nil
	}
	return offer.RegisterCompressionOffers(r, state)
}

func (p metadataStreamingPlugin) WrapCompression(
	w http.ResponseWriter,
	r *http.Request,
	state *compression.State,
	decision compression.Decision,
) (http.ResponseWriter, error) {
	if !metadataFilterMatches(p.filter, r) {
		return w, nil
	}
	offer, ok := p.target.(plugin.CompressionOfferPlugin)
	if !ok {
		return nil, fmt.Errorf("plugin %q has no compression callback", p.target.GetName())
	}
	return offer.WrapCompression(w, r, state, decision)
}

type metadataProtocolPlugin struct{ metadataStreamingPlugin }

func (p metadataProtocolPlugin) RunExclusiveProtocol(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) (base.ProtocolDisposition, *http.Request, ctx.ResponseSource, error) {
	if !metadataFilterMatches(p.filter, r) {
		if next != nil {
			next.ServeHTTP(w, r)
		}
		return base.ProtocolResponded, r, ctx.ResponseSourceUnknown, nil
	}
	terminal, ok := p.target.(base.ExclusiveProtocolTerminal)
	if !ok {
		return 0, r, ctx.ResponseSourceUnknown, fmt.Errorf(
			"plugin %q has no exclusive protocol callback",
			p.target.GetName(),
		)
	}
	return terminal.RunExclusiveProtocol(w, r, next)
}

func (p metadataRequestStorePlugin) RunFinalResponseStore(
	r *http.Request,
	state base.ResponseState,
) error {
	if !metadataFilterMatches(p.filter, r) {
		return nil
	}
	return p.store.RunFinalResponseStore(r, state)
}

func (p metadataRequestStorePlugin) AppliesToResponseSource(
	source ctx.ResponseSource,
) bool {
	return metadataResponseEligible(p.Plugin, source)
}

func metadataFilterMatches(filter *pluginexpr.Expression, r *http.Request) bool {
	return filter == nil || filter.Eval(func(name string) any {
		return pluginexpr.RequestValue(r, name)
	})
}

func metadataSnapshotFilterMatches(filter *pluginexpr.Expression, snapshot base.LogSnapshot) bool {
	return filter == nil || filter.Eval(func(name string) any {
		return pluginexpr.SnapshotValue(snapshot, name)
	})
}

func metadataResponseEligible(p plugin.Plugin, source ctx.ResponseSource) bool {
	if checker, ok := p.(base.ResponseEligibility); ok {
		return checker.AppliesToResponseSource(source)
	}
	return source == ctx.ResponseSourceUpstream
}

const (
	metadataResponseHeader = 1 << iota
	metadataResponseBody
	metadataResponseStore
)

func metadataResponseMask(factoryName string, descriptor plugin.Descriptor) int {
	switch factoryName {
	case "body-transformer":
		if descriptor.HasResponseOwner(plugin.ResponseOwnerBufferedBodyFilter) {
			return metadataResponseBody
		}
	case "echo":
		mask := 0
		if descriptor.HasResponseOwner(plugin.ResponseOwnerHeaderFilter) {
			mask |= metadataResponseHeader
		}
		if descriptor.HasResponseOwner(plugin.ResponseOwnerBufferedBodyFilter) {
			mask |= metadataResponseBody
		}
		return mask
	case "error-page", "exit-transformer", "response-rewrite":
		return metadataResponseBody
	case "proxy-cache", "graphql-proxy-cache":
		return metadataResponseStore
	case "serverless-pre-function", "serverless-post-function":
		mask := 0
		if descriptor.HasResponseOwner(plugin.ResponseOwnerHeaderFilter) {
			mask |= metadataResponseHeader
		}
		if descriptor.HasResponseOwner(plugin.ResponseOwnerBufferedBodyFilter) {
			mask |= metadataResponseBody
		}
		return mask
	default:
		return 0
	}
	return 0
}

func metadataErrorResponseWriter(w http.ResponseWriter, value any) http.ResponseWriter {
	replaced := false
	responseHeaderWritten := false
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(writeHeader httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(status int) {
				if replaced {
					return
				}
				responseHeaderWritten = true
				if status >= http.StatusBadRequest {
					replaced = true
					writeMetadataErrorResponse(w, status, value)
					return
				}
				writeHeader(status)
			}
		},
		Write: func(write httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(body []byte) (int, error) {
				if replaced {
					return len(body), nil
				}
				if !responseHeaderWritten {
					responseHeaderWritten = true
				}
				return write(body)
			}
		},
	})
}

func writeMetadataErrorResponse(w http.ResponseWriter, status int, value any) {
	var body []byte
	contentType := "text/plain; charset=utf-8"
	if object, ok := value.(map[string]any); ok {
		if encoded, err := json.Marshal(object); err == nil {
			body = encoded
			contentType = "application/json"
		}
	} else if text, ok := value.(string); ok {
		body = []byte(text)
	} else if encoded, err := json.Marshal(value); err == nil {
		body = encoded
		contentType = "application/json"
	}
	if body == nil {
		logger.Errorf("marshal metadata error response fail, falling back to text: %v", value)
		body = []byte(fmt.Sprintf("%v", value))
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func parsePluginPriority(value any) (int, error) {
	switch number := value.(type) {
	case int:
		return number, nil
	case int8:
		return int(number), nil
	case int16:
		return int(number), nil
	case int32:
		return int(number), nil
	case int64:
		priority := int(number)
		if int64(priority) == number {
			return priority, nil
		}
	case uint:
		if uint64(number) <= uint64(^uint(0)>>1) {
			return int(number), nil
		}
	case uint8:
		return int(number), nil
	case uint16:
		return int(number), nil
	case uint32:
		if uint64(number) <= uint64(^uint(0)>>1) {
			return int(number), nil
		}
	case uint64:
		if number <= uint64(^uint(0)>>1) {
			return int(number), nil
		}
	case float64:
		if math.Trunc(number) == number {
			priority := int(number)
			if float64(priority) == number {
				return priority, nil
			}
		}
	case json.Number:
		priority, err := strconv.ParseInt(string(number), 10, 64)
		if err == nil {
			return parsePluginPriority(priority)
		}
	}
	return 0, fmt.Errorf("_meta.priority must be an integer")
}

func newMetadataRequestAndBufferedPluginWithDescriptor(
	factoryName string,
	p plugin.Plugin,
	metadata pluginMetadata,
	descriptor plugin.Descriptor,
) (plugin.Plugin, error) {
	if metadata.filter == nil && metadata.errorResponse == nil {
		return p, nil
	}
	basePlugin := metadataPlugin{
		Plugin:        p,
		filter:        metadata.filter,
		errorResponse: metadata.errorResponse,
	}
	responseMask := metadataResponseMask(factoryName, descriptor)
	requestStage := descriptor.RequestStage()
	header, hasHeader := p.(base.HeaderFilterPlugin)
	body, hasBody := p.(base.BufferedBodyFilterPlugin)
	store, hasStore := p.(base.FinalResponseStorePlugin)
	if responseMask&metadataResponseHeader != 0 && !hasHeader {
		return nil, fmt.Errorf("factory %q declares header filter without callback", factoryName)
	}
	if responseMask&metadataResponseBody != 0 && !hasBody {
		return nil, fmt.Errorf("factory %q declares buffered body filter without callback", factoryName)
	}
	if responseMask&metadataResponseStore != 0 && !hasStore {
		return nil, fmt.Errorf("factory %q declares final response store without callback", factoryName)
	}
	if phase, ok := p.(base.RequestPhasePlugin); ok && requestStage != plugin.RequestStageNone {
		requestPlugin := metadataRequestPlugin{
			Plugin:        basePlugin.Plugin,
			phase:         phase,
			filter:        metadata.filter,
			errorResponse: metadata.errorResponse,
		}
		if responseMask&metadataResponseBody != 0 {
			return metadataRequestBodyPlugin{
				metadataRequestPlugin: requestPlugin,
				body:                  body,
			}, nil
		}
		if responseMask&metadataResponseStore != 0 {
			return metadataRequestStorePlugin{
				metadataRequestPlugin: requestPlugin,
				store:                 store,
			}, nil
		}
		return requestPlugin, nil
	}
	if responseMask&metadataResponseHeader != 0 && responseMask&metadataResponseBody != 0 {
		return metadataResponseHeaderBodyPlugin{
			metadataPlugin: basePlugin,
			header:         header,
			body:           body,
		}, nil
	}
	if responseMask&metadataResponseHeader != 0 {
		return metadataResponseHeaderPlugin{
			metadataPlugin: basePlugin,
			header:         header,
		}, nil
	}
	if responseMask&metadataResponseBody != 0 {
		return metadataResponseBodyPlugin{
			metadataPlugin: basePlugin,
			body:           body,
		}, nil
	}
	if responseMask&metadataResponseStore != 0 {
		return metadataResponseStorePlugin{
			metadataPlugin: basePlugin,
			store:          store,
		}, nil
	}
	return basePlugin, nil
}

func serveDubboIfConfiguredCompiled(
	w http.ResponseWriter,
	r *http.Request,
	lb pxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
	retries ...int,
) bool {
	cfg, ok := dubbo_proxy.GetConfig(r)
	if !ok {
		return false
	}

	retryCount := dubboRetryCount(r, retries...)
	nextTarget := nextDubboTarget(r, lb, targets)
	dubbo_proxy.ServeDubboWithRetries(w, r, nextTarget, cfg, retryCount)
	return true
}

func serveHTTPDubboIfConfiguredCompiled(
	w http.ResponseWriter,
	r *http.Request,
	lb pxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
	retries ...int,
) bool {
	cfg, ok := http_dubbo.GetConfig(r)
	if !ok {
		return false
	}

	retryCount := dubboRetryCount(r, retries...)
	nextTarget := nextDubboTarget(r, lb, targets)
	http_dubbo.ServeDubboWithRetries(w, r, nextTarget, cfg, retryCount)
	return true
}

func selectHTTPDubboTarget(
	r *http.Request,
	lb pxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
) (string, error) {
	if override := traffic_split.GetOverride(r); override != nil {
		if override.HealthReporter != nil {
			enriched := pxy.WithHealthReporter(r, override.HealthReporter)
			if enriched != r {
				*r = *enriched
			}
		}
		pxy.SetSelectedTarget(r, override.HealthTarget)
		return override.Host, nil
	}
	if reporter := healthReporter(lb); reporter != nil {
		enriched := pxy.WithHealthReporter(r, reporter)
		if enriched != r {
			*r = *enriched
		}
	}
	target := pxy.NextTarget(lb, r)
	pxy.SetSelectedTarget(r, target)
	compiled, err := resolveCompiledUpstreamTarget(target, targets)
	if err != nil {
		return "", err
	}
	return compiled.host, nil
}

func dubboRetryCount(r *http.Request, retries ...int) int {
	if override := traffic_split.GetOverride(r); override != nil {
		return override.Retries
	}
	if len(retries) > 0 {
		return retries[0]
	}
	return 0
}

func nextDubboTarget(
	r *http.Request,
	lb pxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
) func() (string, error) {
	trafficOverride := traffic_split.GetOverride(r)
	first := true
	return func() (string, error) {
		if trafficOverride != nil {
			if !first {
				if trafficOverride.NextRetry == nil {
					pxy.SetSelectedTarget(r, "")
					return "", fmt.Errorf("traffic-split upstream has no retry target")
				}
				trafficOverride = trafficOverride.NextRetry(r)
				if trafficOverride == nil {
					pxy.SetSelectedTarget(r, "")
					return "", fmt.Errorf("traffic-split upstream has no retry target")
				}
				*r = *traffic_split.WithOverride(r, trafficOverride)
			}
			first = false
		}
		return selectHTTPDubboTarget(r, lb, targets)
	}
}
