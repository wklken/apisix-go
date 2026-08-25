package route

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	defaultTimeout    = 300
	defaultDNSTimeout = 5 * time.Second
)

type Builder struct {
	storage             *store.Store
	serverAddr          string
	staticConfig        *appconfig.EffectiveConfig
	pluginDependencies  base.Dependencies
	enabledPlugins      *plugin.EnabledSet
	clusterRegistry     *pxy.ClusterRegistry
	ownsClusterRegistry bool
	stoppers            []pluginStopper
	stopperMu           sync.Mutex
	consumerResolution  consumerResolutionCache
	servicePlugins      map[servicePluginKey]plugin.Plugin
	servicePluginMu     sync.Mutex
	stopOnce            sync.Once

	// snapshot is the route-build generation for the current Build; it is
	// populated at Build() start and cleared before Build() returns so no
	// request path reads across a generation boundary.
	snapshot *store.ConfigSnapshot
	// snapshotQuarantineCount is published by the server owner only after a
	// successful handler installation, keeping standalone Builder callers free
	// of global readiness side effects.
	snapshotQuarantineCount int
	// compiledSchemas caches plugin schema compilation for the duration of one
	// Build; plugin schemas are constants, so repeated plugin instances never
	// recompile the same schema.
	compiledSchemas map[string]*util.CompiledSchema
}

type consumerResolutionTemplate struct {
	ready    chan struct{}
	bindings []plugin.Binding
	err      error
}

type consumerResolutionCache struct {
	entries sync.Map // map[plugin.ConsumerCacheKey]*consumerResolutionTemplate
}

var errConsumerBindingInitializationPanicked = errors.New("consumer plugin initialization panicked")

type servicePluginKey struct {
	serviceID string
	name      string
	config    string
}

type routeBuildCheckpoint struct {
	publicAPIRegistry public_api.RegistryCheckpoint
	stopperCount      int
	servicePlugins    map[servicePluginKey]plugin.Plugin
}

func NewBuilder(
	storage *store.Store,
	effective *appconfig.EffectiveConfig,
	resolver data_encryption.Resolver,
) *Builder {
	return NewBuilderWithServerAddr(storage, "", effective, resolver)
}

func NewBuilderWithServerAddr(
	storage *store.Store,
	serverAddr string,
	effective *appconfig.EffectiveConfig,
	resolver data_encryption.Resolver,
) *Builder {
	return NewBuilderWithClusterRegistry(
		storage,
		serverAddr,
		pxy.NewClusterRegistry(pxy.NopClusterObserver{}),
		effective,
		resolver,
	)
}

// NewBuilderWithClusterRegistry builds routes against a server-owned cluster
// registry. The builder acquires cluster leases from it and releases them on
// Stop, but never closes it; the server owns its lifecycle.
func NewBuilderWithClusterRegistry(
	storage *store.Store,
	serverAddr string,
	registry *pxy.ClusterRegistry,
	effective *appconfig.EffectiveConfig,
	resolver data_encryption.Resolver,
) *Builder {
	builder := &Builder{
		storage:            storage,
		serverAddr:         normalizeServerAddr(serverAddr),
		staticConfig:       effective,
		pluginDependencies: base.Dependencies{Config: effective, DataEncryption: resolver},
		clusterRegistry:    registry,
		servicePlugins:     make(map[servicePluginKey]plugin.Plugin),
	}
	if registry == nil {
		builder.clusterRegistry = pxy.NewClusterRegistry(pxy.NopClusterObserver{})
		builder.ownsClusterRegistry = true
	}
	return builder
}

func (b *Builder) Stop() {
	b.stopOnce.Do(func() {
		b.stopperMu.Lock()
		stoppers := append([]pluginStopper(nil), b.stoppers...)
		b.stopperMu.Unlock()
		for _, stopper := range stoppers {
			stopper.Stop()
		}
		if b.ownsClusterRegistry && b.clusterRegistry != nil {
			b.clusterRegistry.Close()
		}
	})
}

func (b *Builder) checkpointRouteBuild(publicAPIRegistry *public_api.Registry) routeBuildCheckpoint {
	b.stopperMu.Lock()
	stopperCount := len(b.stoppers)
	b.stopperMu.Unlock()

	b.servicePluginMu.Lock()
	servicePlugins := maps.Clone(b.servicePlugins)
	b.servicePluginMu.Unlock()

	return routeBuildCheckpoint{
		publicAPIRegistry: publicAPIRegistry.Checkpoint(),
		stopperCount:      stopperCount,
		servicePlugins:    servicePlugins,
	}
}

func (b *Builder) rollbackRouteBuild(
	publicAPIRegistry *public_api.Registry,
	checkpoint routeBuildCheckpoint,
) {
	publicAPIRegistry.Rollback(checkpoint.publicAPIRegistry)

	b.servicePluginMu.Lock()
	b.servicePlugins = checkpoint.servicePlugins
	b.servicePluginMu.Unlock()

	b.stopperMu.Lock()
	stoppers := append([]pluginStopper(nil), b.stoppers[checkpoint.stopperCount:]...)
	b.stoppers = b.stoppers[:checkpoint.stopperCount]
	b.stopperMu.Unlock()
	for _, stopper := range slices.Backward(stoppers) {
		stopper.Stop()
	}
}

func (b *Builder) Build() *chi.Mux {
	mux, err := b.BuildStrict()
	if err != nil {
		logger.Errorf("build routes fail: %s", err)
		return nil
	}
	return mux
}

func (b *Builder) BuildStrict() (*chi.Mux, error) {
	return b.buildRoutes(false)
}

// BuildWithRouteQuarantine publishes every valid route while omitting routes
// that fail route-scoped validation, materialization, or URI registration.
// Generation-wide failures remain fatal and are returned to the caller.
func (b *Builder) BuildWithRouteQuarantine() (*chi.Mux, error) {
	return b.buildRoutes(true)
}

func (b *Builder) buildRoutes(quarantineInvalidRoutes bool) (*chi.Mux, error) {
	if err := proxy_cache.RefreshConfiguredZones(b.staticConfig.Config.Apisix.ProxyCache.Zones); err != nil {
		return nil, fmt.Errorf("publish proxy-cache zone registry: %w", err)
	}
	publicAPIRegistry := public_api.NewRegistry()
	if err := registerPrometheusPublicEndpoint(publicAPIRegistry, &b.staticConfig.Config); err != nil {
		return nil, fmt.Errorf("register prometheus public endpoint: %w", err)
	}

	configuredPlugins := b.staticConfig.Config.Plugins
	enabledPlugins := plugin.NewEnabledSet(configuredPlugins)
	b.enabledPlugins = &enabledPlugins

	snapshot, err := b.configSnapshot()
	if err != nil {
		return nil, fmt.Errorf("get config snapshot: %w", err)
	}
	if dynamicPlugins, present := snapshot.HTTPPlugins(); present {
		configuredPlugins = dynamicPlugins
		enabledPlugins = plugin.NewEnabledSet(configuredPlugins)
		b.enabledPlugins = &enabledPlugins
	}
	b.snapshotQuarantineCount = len(snapshot.QuarantinedResources())
	b.snapshot = snapshot
	b.compiledSchemas = make(map[string]*util.CompiledSchema)
	defer func() {
		b.snapshot = nil
		b.compiledSchemas = nil
	}()

	mux := chi.NewRouter()
	mux.Use(pinDecodedRoutePath)
	registrar := newRouteRegistrar(mux)
	routes := normalizeRouteOrder(snapshot.Routes())
	for _, routeResource := range routes {
		if routeResource.Disabled() {
			continue
		}
		var routeCheckpoint routeBuildCheckpoint
		if quarantineInvalidRoutes {
			routeCheckpoint = b.checkpointRouteBuild(publicAPIRegistry)
		}
		uris := routeResource.Uris
		if len(uris) == 0 && routeResource.Uri != "" {
			uris = []string{routeResource.Uri}
		}
		var routeErr error
		effectiveURIs := make(map[string]string, len(uris))
		for _, uri := range uris {
			converted, err := convertURI(uri)
			if err != nil {
				routeErr = fmt.Errorf("register URI %q: %w", uri, err)
				break
			}
			identity := effectiveRouteURI(converted)
			if previous, exists := effectiveURIs[identity]; exists {
				routeErr = fmt.Errorf(
					"duplicate effective URI %q (from %q and %q)",
					identity,
					previous,
					uri,
				)
				break
			}
			effectiveURIs[identity] = uri
		}
		if routeErr == nil {
			var handler http.Handler
			var hosts []string
			handler, hosts, routeErr = b.materializeRouteStrict(routeResource, publicAPIRegistry)
			if routeErr == nil {
				for _, uri := range uris {
					if registerErr := registrar.registerRouteWithHosts(
						routeResource.Methods,
						uri,
						hosts,
						handler,
					); registerErr != nil {
						routeErr = fmt.Errorf("register URI %q: %w", uri, registerErr)
						break
					}
				}
			}
		}
		if routeErr == nil {
			continue
		}
		if !quarantineInvalidRoutes {
			return nil, fmt.Errorf("build route %s: %w", routeResource.ID, routeErr)
		}
		b.rollbackRouteBuild(publicAPIRegistry, routeCheckpoint)
		b.snapshotQuarantineCount++
		logger.Errorf("build route %s fail: %s", routeResource.ID, routeErr)
	}
	notFoundHandler, err := b.buildGlobalNotFoundHandler(snapshot.GlobalRules(), publicAPIRegistry)
	if err != nil {
		return nil, fmt.Errorf("build global not found handler: %w", err)
	}
	mux.NotFound(notFoundHandler.ServeHTTP)
	if err := registerExtraRoutesStrict(mux, &b.staticConfig.Config, publicAPIRegistry); err != nil {
		return nil, fmt.Errorf("register extra routes: %w", err)
	}
	b.configureGlobalErrorLogObserver()
	return mux, nil
}

func (b *Builder) configSnapshot() (*store.ConfigSnapshot, error) {
	if b.storage != nil {
		return b.storage.GetConfigSnapshot()
	}
	return store.GetConfigSnapshot()
}

func (b *Builder) lookupSnapshot() (*store.ConfigSnapshot, error) {
	if b.snapshot != nil {
		return b.snapshot, nil
	}
	if b.storage != nil {
		return b.storage.GetConfigSnapshot()
	}
	return nil, nil
}

func (b *Builder) getService(id string) (resource.Service, error) {
	snapshot, err := b.lookupSnapshot()
	if err != nil {
		return resource.Service{}, err
	}
	if snapshot != nil {
		return snapshot.GetService(id)
	}
	return store.GetService(id)
}

func (b *Builder) getUpstream(id string) (resource.Upstream, error) {
	snapshot, err := b.lookupSnapshot()
	if err != nil {
		return resource.Upstream{}, err
	}
	if snapshot != nil {
		return snapshot.GetUpstream(id)
	}
	return store.GetUpstream(id)
}

func (b *Builder) getPluginConfigRule(id string) (resource.PluginConfigRule, error) {
	snapshot, err := b.lookupSnapshot()
	if err != nil {
		return resource.PluginConfigRule{}, err
	}
	if snapshot != nil {
		return snapshot.GetPluginConfigRule(id)
	}
	return store.GetPluginConfigRule(id)
}

func (b *Builder) getSSL(id string) (resource.SSL, error) {
	snapshot, err := b.lookupSnapshot()
	if err != nil {
		return resource.SSL{}, err
	}
	if snapshot != nil {
		return snapshot.GetSSL(id)
	}
	return store.GetSSL(id)
}

// QuarantinedResourceCount reports malformed legacy snapshot rows plus invalid
// routes omitted by the last quarantining build. The server records this value
// after installing the returned handler.
func (b *Builder) QuarantinedResourceCount() int {
	return b.snapshotQuarantineCount
}

func (b *Builder) buildGlobalNotFoundHandler(
	globalRules []resource.GlobalRule,
	registries ...*public_api.Registry,
) (http.Handler, error) {
	var registry *public_api.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	routeContext := pluginRouteContext{publicAPIRegistry: registry}
	globalRules = deduplicateGlobalRules(globalRules)
	if err := validateSecurityGlobalRulePolicy(b.staticConfig.Profiles, globalRules, ""); err != nil {
		return nil, err
	}
	globalBindings, err := b.initGlobalPluginBindingsStrict(globalRules, routeContext)
	if err != nil {
		return nil, err
	}
	systemBindings, err := b.initPluginBindingsStrict(
		[]materializedPluginSource{{
			name:       "request-context",
			config:     buildRequestContextConfig(resource.Route{}, resource.Service{}),
			scope:      plugin.ScopeSystem,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
		}},
		routeContext,
		pluginInitOptions{allowRequestContext: true},
	)
	if err != nil {
		return nil, err
	}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx.SetRequestResponseSource(r, ctx.ResponseSourceEarlyStop)
		http.NotFoundHandler().ServeHTTP(w, r)
	})
	staticBindings := append(append([]plugin.Binding{}, systemBindings...), globalBindings...)
	plan, err := plugin.BuildResponsePlan(plugin.ResponsePlanInput{
		StaticBindings: staticBindings,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		return nil, err
	}
	pipeline, err := newRequestPipelineWithLog(staticBindings, nil)
	if err != nil {
		return nil, err
	}
	return ensureRouteLifecycle(plan.Install(pipeline, terminal)), nil
}

func normalizePluginResourceContext(
	context pluginRouteContext,
	name string,
	config resource.PluginConfig,
) pluginRouteContext {
	if _, ok := context.route.Plugins[name]; ok {
		context.route.Plugins = clonePluginConfigs(context.route.Plugins)
		context.route.Plugins[name] = config
		return context
	}
	if _, ok := context.service.Plugins[name]; ok {
		context.service.Plugins = clonePluginConfigs(context.service.Plugins)
		context.service.Plugins[name] = config
	}
	return context
}

func (b *Builder) buildHandlerStrict(
	r resource.Route,
	registries ...*public_api.Registry,
) (http.Handler, error) {
	if err := validateRouteCompatibility(r); err != nil {
		return nil, err
	}
	service, err := b.loadRouteService(r)
	if err != nil {
		return nil, err
	}
	return b.buildHandlerWithServiceStrict(r, service, registries...)
}

func (b *Builder) materializeRouteStrict(
	r resource.Route,
	registries ...*public_api.Registry,
) (http.Handler, []string, error) {
	if err := validateRouteCompatibility(r); err != nil {
		return nil, nil, err
	}
	service, err := b.loadRouteService(r)
	if err != nil {
		return nil, nil, err
	}
	handler, err := b.buildHandlerWithServiceStrict(r, service, registries...)
	if err != nil {
		return nil, nil, err
	}
	hosts := r.EffectiveHosts()
	if !r.HostConfigured() && !r.HostsConfigured() && r.ServiceID != "" {
		hosts = service.Hosts
	}
	return handler, hosts, nil
}

func (b *Builder) loadRouteService(r resource.Route) (resource.Service, error) {
	if r.ServiceID == "" {
		return resource.Service{}, nil
	}
	service, err := b.getService(r.ServiceID)
	if err != nil {
		logger.Errorf("get service fail: %s", err)
		return resource.Service{}, err
	}
	return service, nil
}

func (b *Builder) buildHandlerWithServiceStrict(
	r resource.Route,
	service resource.Service,
	registries ...*public_api.Registry,
) (http.Handler, error) {
	selection := b.staticConfig.Profiles
	var pluginConfigPlugins map[string]resource.PluginConfig
	// handle plugin_config_id
	if r.PluginConfigID != "" {
		pluginConfigRule, err := b.getPluginConfigRule(r.PluginConfigID)
		if err != nil {
			// FIXME: should return 503
			logger.Errorf("get plugin config rule fail: %s", err)
			return nil, err
		}
		if err := b.validatePluginConfigSource(
			pluginConfigRule.Plugins,
			"plugin_config",
			r.PluginConfigID,
		); err != nil {
			return nil, err
		}
		pluginConfigPlugins = pluginConfigRule.Plugins
	}

	if r.ServiceID != "" {
		if err := b.validatePluginConfigSource(service.Plugins, "service", r.ServiceID); err != nil {
			return nil, err
		}
	}
	if err := b.validatePluginConfigSource(r.Plugins, "route", r.ID); err != nil {
		return nil, err
	}

	localSources, serviceSources, _ := selectMaterializedPluginSources(
		r.Plugins,
		r.ID,
		pluginConfigPlugins,
		r.PluginConfigID,
		service.Plugins,
		r.ServiceID,
	)
	if err := validateSecurityMaterializedPluginSources(
		selection,
		append(localSources, serviceSources...),
		r.ID,
	); err != nil {
		return nil, err
	}

	// add a context plugin, set the default vars
	systemPlugins := buildSystemPluginConfigs(r, service)

	routeContext := b.pluginRouteContext(r)
	if len(registries) > 0 {
		routeContext.publicAPIRegistry = registries[0]
	}
	routeContext.service = service
	localBindings, err := b.initPluginBindingsStrict(localSources, routeContext, pluginInitOptions{})
	if err != nil {
		return nil, err
	}
	serviceBindings, err := b.initServicePluginBindingsStrict(serviceSources, routeContext)
	if err != nil {
		return nil, err
	}
	systemSources := materializedPluginSources(
		systemPlugins,
		plugin.ResourceProvenance{Kind: plugin.ResourceSystem},
	)
	for i := range systemSources {
		systemSources[i].scope = plugin.ScopeSystem
		systemSources[i].provenance.ID = systemSources[i].name
	}
	systemBindings, err := b.initPluginBindingsStrict(
		systemSources,
		routeContext,
		pluginInitOptions{allowRequestContext: true},
	)
	if err != nil {
		return nil, err
	}
	globalRules, err := b.globalRules()
	if err != nil {
		logger.Errorf("list global rules fail: %s", err)
		return nil, err
	}
	globalRules = deduplicateGlobalRules(globalRules)
	if err := validateSecurityGlobalRulePolicy(selection, globalRules, r.ID); err != nil {
		return nil, err
	}
	globalBindings, err := b.initGlobalPluginBindingsStrict(globalRules, routeContext)
	if err != nil {
		return nil, err
	}
	localBindings = append(localBindings, serviceBindings...)
	terminalSources := append([]materializedPluginSource(nil), localSources...)
	terminalSources = append(terminalSources, serviceSources...)
	for _, rule := range globalRules {
		globalSources := materializedPluginSources(
			rule.Plugins,
			plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: rule.ID},
		)
		for i := range globalSources {
			globalSources[i].scope = plugin.ScopeGlobal
		}
		terminalSources = append(terminalSources, globalSources...)
	}

	resolvedUpstream, upstreamProvenance, err := b.resolveRouteUpstream(r, service)
	if err != nil {
		return nil, err
	}
	if err := validateSecurityUpstreamPolicy(
		selection,
		resolvedUpstream,
		fmt.Sprintf("%s %q for route %q", upstreamProvenance.Kind, upstreamProvenance.ID, r.ID),
	); err != nil {
		return nil, fmt.Errorf("route %q: %w", r.ID, err)
	}
	if err := validateUnsupportedUpstreamDiscovery(resolvedUpstream, upstreamProvenance); err != nil {
		return nil, err
	}
	handler, routeTerminals, err := b.buildReverseHandlerWithTerminals(r, service, resolvedUpstream)
	if err != nil {
		logger.Errorf("build reverse handler fail: %s", err)
		return nil, err
	}

	staticBindings := make([]plugin.Binding, 0, len(systemBindings)+len(globalBindings)+len(localBindings))
	staticBindings = append(staticBindings, systemBindings...)
	staticBindings = append(staticBindings, globalBindings...)
	staticBindings = append(staticBindings, localBindings...)
	responseTerminals, err := routeTerminalCandidates(
		terminalSources,
		resolvedUpstream,
		upstreamProvenance,
		routeTerminals,
	)
	if err != nil {
		return nil, err
	}
	plan, err := plugin.BuildResponsePlan(plugin.ResponsePlanInput{
		StaticBindings: staticBindings,
		RouteTerminals: responseTerminals,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		return nil, err
	}
	pipeline, err := newRequestPipelineWithLog(staticBindings, b.resolveConsumerBindings(routeContext))
	if err != nil {
		return nil, err
	}
	websocketEnabled := r.EnableWebsocket
	if !r.EnableWebsocketConfigured() {
		websocketEnabled = service.EnableWebsocket
	}
	ordinaryHandler := plan.Install(pipeline, requireWebsocketEnablement(handler, websocketEnabled))
	transparentUpgradeHandler, err := buildTransparentUpgradeHandler(
		pipeline, plan, handler, websocketEnabled,
	)
	if err != nil {
		return nil, err
	}
	return ensureRouteLifecycle(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if isWebsocketUpgradeRequest(request) {
			transparentUpgradeHandler.ServeHTTP(w, request)
			return
		}
		ordinaryHandler.ServeHTTP(w, request)
	})), nil
}

func (b *Builder) resolveConsumerBindings(
	routeContext pluginRouteContext,
) plugin.ConsumerBindingResolver {
	return func(request *http.Request) (plugin.ConsumerResolution, error) {
		resolution := plugin.ConsumerResolution{Request: request}
		state, ok := ctx.AuthenticationStateFrom(request)
		if !ok {
			return resolution, nil
		}

		consumer := state.Consumer()
		resolution.Identity = plugin.ConsumerIdentity{
			Username:   consumer.Username,
			GroupID:    consumer.GroupID,
			AuthSource: state.Source,
		}

		var group resource.ConsumerGroup
		if consumer.GroupID != "" {
			var err error
			group, err = store.GetConsumerGroup(consumer.GroupID)
			if err != nil {
				return resolution, fmt.Errorf(
					"resolve consumer %q group %q: %w",
					consumer.Username,
					consumer.GroupID,
					err,
				)
			}
			if err := b.validatePluginConfigSource(group.Plugins, "consumer_group", consumer.GroupID); err != nil {
				return resolution, err
			}
		}
		if err := b.validatePluginConfigSource(consumer.Plugins, "consumer", consumer.Username); err != nil {
			return resolution, err
		}

		consumerDigest, err := consumerConfigDigest(consumer.Plugins, consumer.ConfigDigest)
		if err != nil {
			return resolution, fmt.Errorf("resolve consumer %q: %w", consumer.Username, err)
		}
		var groupDigest [32]byte
		if consumer.GroupID != "" {
			groupDigest, err = consumerConfigDigest(group.Plugins, group.ConfigDigest)
			if err != nil {
				return resolution, fmt.Errorf("resolve consumer group %q: %w", consumer.GroupID, err)
			}
		}
		serviceID := routeContext.service.ID
		if serviceID == "" {
			serviceID = routeContext.route.ServiceID
		}
		key := plugin.ConsumerCacheKey{
			ConsumerID:     consumer.Username,
			ConsumerDigest: consumerDigest,
			GroupID:        consumer.GroupID,
			GroupDigest:    groupDigest,
			RouteID:        routeContext.routeID,
			ServiceID:      serviceID,
		}
		resolution.CacheKey = key

		bindings, err := b.consumerBindingsForKey(key, func() ([]plugin.Binding, error) {
			sources := consumerPluginSources(group, consumer)
			return b.initPluginBindingsStrict(sources, routeContext, pluginInitOptions{})
		})
		if err != nil {
			return resolution, err
		}

		request = ctx.WithApisixVars(request, nil)
		ctx.AttachConsumer(request, consumer)
		overrides := make(map[string]struct{}, len(bindings))
		for _, binding := range bindings {
			if binding.Plugin != nil {
				overrides[binding.Plugin.GetName()] = struct{}{}
			}
		}
		request = ctx.WithConsumerPluginOverrides(request, overrides)
		resolution.Bindings = append([]plugin.Binding(nil), bindings...)
		resolution.Request = request
		resolution.Resolved = true
		return resolution, nil
	}
}

func (b *Builder) consumerBindingsForKey(
	key plugin.ConsumerCacheKey,
	initialize func() ([]plugin.Binding, error),
) ([]plugin.Binding, error) {
	if actual, ok := b.consumerResolution.entries.Load(key); ok {
		template := actual.(*consumerResolutionTemplate)
		<-template.ready
		if template.err != nil {
			return nil, template.err
		}
		return append([]plugin.Binding(nil), template.bindings...), nil
	}

	template := &consumerResolutionTemplate{ready: make(chan struct{})}
	actual, loaded := b.consumerResolution.entries.LoadOrStore(key, template)
	if loaded {
		template = actual.(*consumerResolutionTemplate)
		<-template.ready
		if template.err != nil {
			return nil, template.err
		}
		return append([]plugin.Binding(nil), template.bindings...), nil
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			template.err = errConsumerBindingInitializationPanicked
			close(template.ready)
			b.consumerResolution.entries.Delete(key)
			panic(recovered)
		}
	}()
	bindings, err := initialize()
	template.bindings = append([]plugin.Binding(nil), bindings...)
	template.err = err
	close(template.ready)
	if err != nil {
		b.consumerResolution.entries.Delete(key)
	}
	if err != nil {
		return nil, err
	}
	return append([]plugin.Binding(nil), template.bindings...), nil
}

func consumerConfigDigest(configs map[string]resource.PluginConfig, configured [32]byte) ([32]byte, error) {
	if configured != ([32]byte{}) {
		return configured, nil
	}
	encoded, err := json.Marshal(configs)
	if err != nil {
		return [32]byte{}, fmt.Errorf("marshal plugin configs: %w", err)
	}
	return sha256.Sum256(encoded), nil
}

func pluginsFromBindings(bindings []plugin.Binding) []plugin.Plugin {
	plugins := make([]plugin.Plugin, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Plugin != nil {
			plugins = append(plugins, binding.Plugin)
		}
	}
	return plugins
}

type pluginRouteContext struct {
	routeID           string
	serverAddr        string
	route             resource.Route
	service           resource.Service
	publicAPIRegistry *public_api.Registry
}

func (b *Builder) pluginRouteContext(r resource.Route) pluginRouteContext {
	return pluginRouteContext{
		routeID:    r.ID,
		serverAddr: b.serverAddr,
		route:      r,
	}
}

func normalizeServerAddr(serverAddr string) string {
	if strings.HasPrefix(serverAddr, ":") {
		return "0.0.0.0" + serverAddr
	}
	return serverAddr
}

type pluginRouteContextSetter interface {
	SetRouteContext(routeID string, serverAddr string)
}

type pluginResourceContextSetter interface {
	SetResourceContext(route resource.Route, service resource.Service)
}

type pluginTrafficSplitRuntimeAcquirerSetter interface {
	SetRuntimeAcquirer(traffic_split.RuntimeAcquirer)
}

type pluginTrafficSplitUpstreamResolverSetter interface {
	SetUpstreamResolver(traffic_split.ResourceUpstreamResolver)
}

type trafficSplitRuntimeAcquirer struct {
	builder *Builder
	route   resource.Route
}

func (a *trafficSplitRuntimeAcquirer) Acquire(
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
) (*traffic_split.Runtime, error) {
	if upstream == nil {
		return nil, fmt.Errorf("traffic-split upstream is nil")
	}
	if a.builder.clusterRegistry == nil {
		a.builder.clusterRegistry = pxy.NewClusterRegistry(pxy.NopClusterObserver{})
		a.builder.ownsClusterRegistry = true
	}
	clusterConfig, err := planTrafficSplitClusterWithSSLResolver(
		a.route, upstream, targets, priorities, a.builder.getSSL, &a.builder.staticConfig.Config,
	)
	if err != nil {
		return nil, err
	}
	lease, err := a.builder.clusterRegistry.Acquire(clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("acquire traffic-split upstream cluster: %w", err)
	}
	a.builder.addStopper(lease)
	cluster := lease.Cluster()
	return &traffic_split.Runtime{
		LoadBalancer: cluster.LoadBalancer(),
		RoundTripper: cluster.RoundTripper(),
	}, nil
}

type pluginEnabledCheckerSetter interface {
	SetPluginEnabledChecker(func(string) bool)
}

type publicAPIRegistrySetter interface {
	SetPublicAPIRegistry(*public_api.Registry)
}

type pluginPreMaterializationValidator interface {
	ValidatePreMaterialization() error
}

type pluginInitOptions struct {
	allowRequestContext bool
}

func (b *Builder) validatePluginConfigSource(
	pluginConfigs map[string]resource.PluginConfig,
	kind string,
	id string,
) error {
	if b.enabledPlugins == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(pluginConfigs)) {
		if !b.enabledPlugins.Contains(name) {
			return fmt.Errorf("plugin %q is disabled in %s %q", name, kind, id)
		}
	}
	return nil
}

type pluginStopper interface {
	Stop()
}

type pluginObserverStarter interface {
	StartObserving()
}

func (b *Builder) configureGlobalErrorLogObserver() {
	const pluginName = "error-log-logger"
	enabled := false
	if b.enabledPlugins != nil {
		enabled = b.enabledPlugins.Contains(pluginName)
	} else {
		enabled = slices.Contains(b.staticConfig.Config.Plugins, pluginName)
	}
	if !enabled {
		_ = logger.ReplaceObserver(pluginName, nil)
		return
	}

	var metadata map[string]any
	if metadata, ok := b.pluginMetadata(pluginName); !ok || len(metadata) == 0 {
		_ = logger.ReplaceObserver(pluginName, nil)
		logger.Errorf("please set the correct plugin_metadata for error-log-logger")
		return
	}
	if err := b.startGlobalErrorLogObserver(metadata); err != nil {
		_ = logger.ReplaceObserver(pluginName, nil)
		logger.Errorf("please set the correct plugin_metadata for error-log-logger: %s", err)
	}
}

func (b *Builder) startGlobalErrorLogObserver(config resource.PluginConfig) error {
	const pluginName = "error-log-logger"
	p := plugin.New(pluginName, b.pluginDependencies)
	if p == nil {
		return fmt.Errorf("plugin %s is not supported", pluginName)
	}
	starter, ok := p.(pluginObserverStarter)
	if !ok {
		return fmt.Errorf("plugin %s does not support global observation", pluginName)
	}
	if err := p.Init(); err != nil {
		return fmt.Errorf("initialize plugin %s: %w", pluginName, err)
	}
	compiledSchema, err := util.CompileSchema(p.GetSchema())
	if err != nil {
		return fmt.Errorf("validate plugin %s metadata: %w", pluginName, err)
	}
	if err := compiledSchema.Validate(config); err != nil {
		return fmt.Errorf("validate plugin %s metadata: %w", pluginName, err)
	}
	if err := util.Parse(config, p.Config()); err != nil {
		return fmt.Errorf("parse plugin %s metadata: %w", pluginName, err)
	}
	if err := plugin.MaterializePluginSecrets(p); err != nil {
		return fmt.Errorf("materialize plugin %s secrets: %w", pluginName, err)
	}
	_, cleanupMaterializedSecrets := p.(plugin.SecretMaterializer)
	defer func() {
		if cleanupMaterializedSecrets {
			if stopper, ok := p.(pluginStopper); ok {
				stopper.Stop()
			}
		}
	}()
	if err := p.PostInit(); err != nil {
		return fmt.Errorf("initialize plugin %s: %w", pluginName, err)
	}
	starter.StartObserving()
	if stopper, ok := p.(pluginStopper); ok {
		b.addStopper(stopper)
	}
	cleanupMaterializedSecrets = false
	return nil
}

// globalRules returns the global rules of the current build generation,
// falling back to a live store read outside Build.
func (b *Builder) globalRules() ([]resource.GlobalRule, error) {
	snapshot, err := b.lookupSnapshot()
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		return snapshot.GlobalRules(), nil
	}
	return store.ListGlobalRules()
}

// pluginMetadata returns the decoded plugin metadata of the current build
// generation, falling back to a live store read outside Build.
func (b *Builder) pluginMetadata(name string) (map[string]any, bool) {
	if snapshot, err := b.lookupSnapshot(); err == nil && snapshot != nil {
		return snapshot.PluginMetadata(name)
	}
	var metadata map[string]any
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return nil, false
	}
	return metadata, true
}

// compiledSchema returns the compiled form of schema, caching the result for
// the duration of one Build. Plugin schemas are constants, so the same schema
// string always compiles to equivalent validation behavior.
func (b *Builder) compiledSchema(schema string) (*util.CompiledSchema, error) {
	if b.compiledSchemas != nil {
		if compiled, ok := b.compiledSchemas[schema]; ok {
			return compiled, nil
		}
	}
	compiled, err := util.CompileSchema(schema)
	if err != nil {
		return nil, err
	}
	if b.compiledSchemas != nil {
		b.compiledSchemas[schema] = compiled
	}
	return compiled, nil
}

func (b *Builder) initPlugins(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
) []plugin.Plugin {
	plugins, err := b.initPluginsStrict(pluginConfigs, routeContext)
	if err != nil {
		logger.Errorf("initialize strict plugin set fail: %s", err)
	}
	return plugins
}

func (b *Builder) initServicePluginsStrict(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
) ([]plugin.Plugin, error) {
	sources := materializedPluginSources(pluginConfigs, plugin.ResourceProvenance{})
	bindings, err := b.initServicePluginBindingsStrict(sources, routeContext)
	return pluginsFromBindings(bindings), err
}

func (b *Builder) initServicePluginBindingsStrict(
	sources []materializedPluginSource,
	routeContext pluginRouteContext,
) ([]plugin.Binding, error) {
	bindings := make([]plugin.Binding, 0, len(sources))
	for _, source := range sources {
		if source.name != "kafka-logger" || routeContext.service.ID == "" {
			initialized, err := b.initPluginBindingsStrict(
				[]materializedPluginSource{source},
				routeContext,
				pluginInitOptions{},
			)
			if err != nil {
				return nil, err
			}
			bindings = append(bindings, initialized...)
			continue
		}

		encoded, err := json.Marshal(source.config)
		if err != nil {
			if source.provenance.Kind == "" {
				return nil, fmt.Errorf("marshal service plugin %s config: %w", source.name, err)
			}
			return nil, fmt.Errorf(
				"plugin %q from %s %q: marshal service plugin %s config: %w",
				source.name,
				source.provenance.Kind,
				source.provenance.ID,
				source.name,
				err,
			)
		}
		key := servicePluginKey{
			serviceID: routeContext.service.ID,
			name:      source.name,
			config:    string(encoded),
		}

		b.servicePluginMu.Lock()
		initialized := b.servicePlugins[key]
		if initialized == nil {
			servicePlugins, initErr := b.initPluginBindingsStrict(
				[]materializedPluginSource{source},
				routeContext,
				pluginInitOptions{},
			)
			if initErr != nil {
				b.servicePluginMu.Unlock()
				return nil, initErr
			}
			if len(servicePlugins) == 1 {
				initialized = servicePlugins[0].Plugin
				b.servicePlugins[key] = initialized
			}
		}
		b.servicePluginMu.Unlock()
		if initialized != nil {
			_, metadata, metadataErr := parsePluginMetadata(source.config)
			if metadataErr != nil {
				return nil, metadataErr
			}
			descriptor, descriptorErr := plugin.ResolveDescriptorForFactory(source.name, initialized)
			if descriptorErr != nil {
				return nil, descriptorErr
			}
			binding, bindErr := plugin.BindResolvedPlugin(
				descriptor,
				initialized,
				source.scope,
				source.provenance,
				metadata.instanceIdentity(initialized.Config()),
			)
			if bindErr != nil {
				return nil, bindErr
			}
			if metadata.priority != nil {
				binding.Priority = *metadata.priority
			}
			bindings = append(bindings, binding)
		}
	}
	return bindings, nil
}

func (b *Builder) initPluginsStrict(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
) ([]plugin.Plugin, error) {
	return b.initPluginsStrictWithOptions(pluginConfigs, routeContext, pluginInitOptions{})
}

func (b *Builder) initPluginsStrictWithOptions(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
	options pluginInitOptions,
) ([]plugin.Plugin, error) {
	sources := materializedPluginSources(pluginConfigs, plugin.ResourceProvenance{})
	bindings, err := b.initPluginBindingsStrict(sources, routeContext, options)
	return pluginsFromBindings(bindings), err
}

func (b *Builder) initPluginBindingsStrict(
	sources []materializedPluginSource,
	routeContext pluginRouteContext,
	options pluginInitOptions,
) ([]plugin.Binding, error) {
	bindings := make([]plugin.Binding, 0, len(sources))
	normalizedRouteContext := routeContext
	resourceContextSetters := make([]pluginResourceContextSetter, 0, len(sources))
	pendingStoppers := make([]pluginStopper, 0, len(sources))
	committed := false
	defer func() {
		if committed {
			return
		}
		for _, stopper := range slices.Backward(pendingStoppers) {
			stopper.Stop()
		}
	}()
	for _, source := range sources {
		name := source.name
		config := source.config
		sourceError := func(err error) error {
			if source.provenance.Kind == "" {
				return err
			}
			return fmt.Errorf(
				"plugin %q from %s %q: %w",
				name,
				source.provenance.Kind,
				source.provenance.ID,
				err,
			)
		}
		if !b.pluginAllowed(name, options) {
			return nil, sourceError(fmt.Errorf("plugin %q is disabled", name))
		}
		p := plugin.New(name, b.pluginDependencies)
		if p == nil {
			return nil, sourceError(fmt.Errorf("plugin %s is not supported", name))
		}
		config, metadata, err := parsePluginMetadata(config)
		if err != nil {
			return nil, sourceError(fmt.Errorf("parse plugin %s metadata: %w", name, err))
		}
		if metadata.disabled {
			continue
		}

		if err := p.Init(); err != nil {
			return nil, sourceError(fmt.Errorf("initialize plugin %s: %w", name, err))
		}
		if metadataSchema := p.GetMetadataSchema(); metadataSchema != "" {
			if metadata, ok := b.pluginMetadata(name); ok {
				compiledMetadataSchema, compileErr := b.compiledSchema(metadataSchema)
				if compileErr != nil {
					return nil, sourceError(fmt.Errorf("validate plugin %s metadata: %w", name, compileErr))
				}
				if err := compiledMetadataSchema.Validate(metadata); err != nil {
					return nil, sourceError(fmt.Errorf("validate plugin %s metadata: %w", name, err))
				}
			}
		}

		compiledSchema, compileErr := b.compiledSchema(p.GetSchema())
		if compileErr != nil {
			return nil, sourceError(fmt.Errorf("validate plugin %s config: %w", name, compileErr))
		}
		err = compiledSchema.Validate(config)
		if err != nil {
			return nil, sourceError(fmt.Errorf("validate plugin %s config: %w", name, err))
		}

		err = util.Parse(config, p.Config())
		if err != nil {
			return nil, sourceError(fmt.Errorf("parse plugin %s config: %w", name, err))
		}
		if setter, ok := p.(pluginEnabledCheckerSetter); ok && b.enabledPlugins != nil {
			checker := b.enabledPlugins.Contains
			setter.SetPluginEnabledChecker(checker)
		}
		if setter, ok := p.(publicAPIRegistrySetter); ok {
			setter.SetPublicAPIRegistry(routeContext.publicAPIRegistry)
		}
		if validator, ok := p.(pluginPreMaterializationValidator); ok {
			if err := validator.ValidatePreMaterialization(); err != nil {
				return nil, sourceError(fmt.Errorf("validate plugin %s before secret materialization: %w", name, err))
			}
		}
		if err := plugin.MaterializePluginSecrets(p); err != nil {
			return nil, sourceError(fmt.Errorf("materialize plugin %s secrets: %w", name, err))
		}
		secretStopperAdded := false
		if _, ownsSecrets := p.(plugin.SecretMaterializer); ownsSecrets {
			if stopper, ok := p.(pluginStopper); ok {
				pendingStoppers = append(pendingStoppers, stopper)
				secretStopperAdded = true
			}
		}

		if setter, ok := p.(pluginRouteContextSetter); ok {
			setter.SetRouteContext(routeContext.routeID, routeContext.serverAddr)
		}
		if setter, ok := p.(pluginResourceContextSetter); ok {
			setter.SetResourceContext(routeContext.route, routeContext.service)
			resourceContextSetters = append(resourceContextSetters, setter)
		}
		if setter, ok := p.(pluginTrafficSplitRuntimeAcquirerSetter); ok {
			setter.SetRuntimeAcquirer(&trafficSplitRuntimeAcquirer{builder: b, route: routeContext.route})
		}
		if setter, ok := p.(pluginTrafficSplitUpstreamResolverSetter); ok {
			setter.SetUpstreamResolver(b.getUpstream)
		}
		if err := p.PostInit(); err != nil {
			return nil, sourceError(fmt.Errorf("initialize plugin %s: %w", name, err))
		}
		normalizedRouteContext = normalizePluginResourceContext(normalizedRouteContext, name, p.Config())
		if stopper, ok := p.(pluginStopper); ok && !secretStopperAdded {
			pendingStoppers = append(pendingStoppers, stopper)
		}

		descriptor, descriptorErr := plugin.ResolveDescriptorForFactory(name, p)
		if descriptorErr != nil {
			return nil, sourceError(descriptorErr)
		}
		initialized, metadataErr := newMetadataPluginWithDescriptor(name, p, metadata, descriptor)
		if metadataErr != nil {
			return nil, sourceError(metadataErr)
		}
		binding, bindErr := plugin.BindResolvedPlugin(
			descriptor,
			initialized,
			source.scope,
			source.provenance,
			metadata.instanceIdentity(p.Config()),
		)
		if bindErr != nil {
			return nil, sourceError(bindErr)
		}
		if metadata.priority != nil {
			binding.Priority = *metadata.priority
		}
		bindings = append(bindings, binding)
	}
	for _, setter := range resourceContextSetters {
		setter.SetResourceContext(normalizedRouteContext.route, normalizedRouteContext.service)
	}
	for _, stopper := range pendingStoppers {
		b.addStopper(stopper)
	}
	committed = true
	return bindings, nil
}

func (b *Builder) pluginAllowed(name string, options pluginInitOptions) bool {
	if b.enabledPlugins == nil {
		return true
	}
	if options.allowRequestContext && name == "request-context" {
		return true
	}
	return b.enabledPlugins.Contains(name)
}

func (b *Builder) addStopper(stopper pluginStopper) {
	b.stopperMu.Lock()
	b.stoppers = append(b.stoppers, stopper)
	b.stopperMu.Unlock()
}

func (b *Builder) initGlobalPluginsStrict(
	globalRules []resource.GlobalRule,
	routeContext pluginRouteContext,
) ([]plugin.Plugin, error) {
	bindings, err := b.initGlobalPluginBindingsStrict(globalRules, routeContext)
	return pluginsFromBindings(bindings), err
}

func (b *Builder) initGlobalPluginBindingsStrict(
	globalRules []resource.GlobalRule,
	routeContext pluginRouteContext,
) ([]plugin.Binding, error) {
	globalRules = deduplicateGlobalRules(globalRules)
	bindings := make([]plugin.Binding, 0, len(globalRules))
	for _, rule := range globalRules {
		if rule.ID == "" {
			return nil, fmt.Errorf("global rule ID is required for scoped plugin binding")
		}
		if err := b.validatePluginConfigSource(rule.Plugins, "global_rule", rule.ID); err != nil {
			return nil, err
		}
		sources := materializedPluginSources(
			rule.Plugins,
			plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: rule.ID},
		)
		for i := range sources {
			sources[i].scope = plugin.ScopeGlobal
		}
		initialized, err := b.initPluginBindingsStrict(sources, routeContext, pluginInitOptions{})
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, initialized...)
	}
	return bindings, nil
}

func resolveRouteUpstream(
	r resource.Route,
	service resource.Service,
) (resource.Upstream, plugin.ResourceProvenance, error) {
	return resolveRouteUpstreamWithGetter(r, service, store.GetUpstream)
}

func (b *Builder) resolveRouteUpstream(
	r resource.Route,
	service resource.Service,
) (resource.Upstream, plugin.ResourceProvenance, error) {
	return resolveRouteUpstreamWithGetter(r, service, b.getUpstream)
}

func routeTerminalCandidates(
	sources []materializedPluginSource,
	upstream resource.Upstream,
	upstreamProvenance plugin.ResourceProvenance,
	terminals routeProtocolTerminals,
) ([]plugin.RouteTerminalCandidate, error) {
	candidates := make([]plugin.RouteTerminalCandidate, 0, 1)
	seen := make(map[plugin.ProtocolKind]bool)
	for _, source := range sources {
		_, metadata, err := parsePluginMetadata(source.config)
		if err != nil {
			return nil, fmt.Errorf(
				"plugin %q from %s %q: parse plugin metadata: %w",
				source.name,
				source.provenance.Kind,
				source.provenance.ID,
				err,
			)
		}
		if metadata.disabled {
			continue
		}
		identity := source.name
		var protocol plugin.ProtocolKind
		var terminal base.ExclusiveProtocolTerminal
		switch identity {
		case "dubbo-proxy":
			protocol, terminal = plugin.ProtocolDubbo, terminals.dubbo
		case "http-dubbo":
			protocol, terminal = plugin.ProtocolHTTPDubbo, terminals.httpDubbo
		case "kafka-proxy":
			protocol, terminal = plugin.ProtocolKafka, terminals.kafka
		default:
			continue
		}
		if terminal == nil || seen[protocol] {
			continue
		}
		seen[protocol] = true
		candidates = append(candidates, plugin.RouteTerminalCandidate{
			Identity: identity, Scope: source.scope,
			Provenance: source.provenance, Protocol: protocol, Terminal: terminal,
		})
	}
	if strings.EqualFold(upstream.Scheme, "kafka") && terminals.kafka != nil && !seen[plugin.ProtocolKafka] {
		candidates = append(candidates, plugin.RouteTerminalCandidate{
			Identity: "kafka-proxy", Scope: plugin.ScopeRoute, Priority: 0,
			Provenance: upstreamProvenance, Protocol: plugin.ProtocolKafka, Terminal: terminals.kafka,
		})
	}
	return candidates, nil
}

// buildReverseHandler retains the direct helper contract used by isolated
// callers. Route generations use buildReverseHandlerWithTerminals so protocol
// ownership is installed exactly once through ResponsePlan.
func (b *Builder) buildReverseHandler(
	r resource.Route,
	service resource.Service,
	resolved ...resource.Upstream,
) (http.Handler, error) {
	handler, terminals, err := b.buildReverseHandlerWithTerminals(r, service, resolved...)
	if err != nil {
		return nil, err
	}
	if terminals.kafka == nil && terminals.dubbo == nil && terminals.httpDubbo == nil {
		return handler, nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var terminal base.ExclusiveProtocolTerminal
		switch {
		case terminals.kafka != nil:
			terminal = terminals.kafka
		case terminals.dubbo != nil:
			if _, ok := dubbo_proxy.GetConfig(r); !ok {
				handler.ServeHTTP(w, r)
				return
			}
			terminal = terminals.dubbo
		case terminals.httpDubbo != nil:
			if _, ok := http_dubbo.GetConfig(r); !ok {
				handler.ServeHTTP(w, r)
				return
			}
			terminal = terminals.httpDubbo
		}
		if terminal != nil {
			_, _, _, _ = terminal.RunExclusiveProtocol(w, r, nil)
			return
		}
		handler.ServeHTTP(w, r)
	}), nil
}

func (b *Builder) buildReverseHandlerWithTerminals(
	r resource.Route,
	service resource.Service,
	resolved ...resource.Upstream,
) (http.Handler, routeProtocolTerminals, error) {
	var upstream resource.Upstream
	upstreamProvenance := plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: r.ID}
	if len(resolved) > 0 {
		upstream = resolved[0]
		switch {
		case r.UpstreamID != "":
			upstreamProvenance = plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: r.UpstreamID}
		case service.UpstreamID != "":
			upstreamProvenance = plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: service.UpstreamID}
		}
	} else {
		var err error
		upstream, upstreamProvenance, err = b.resolveRouteUpstream(r, service)
		if err != nil {
			return nil, routeProtocolTerminals{}, err
		}
	}
	if err := validateUnsupportedUpstreamDiscovery(upstream, upstreamProvenance); err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	if err := validateHTTPUpstreamType(upstream); err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	if err := validatePlannedPassHost(upstream); err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	servers, priorities, err := planUpstreamNodes(upstream)
	if err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	scheme := upstream.Scheme
	compiledTargets, err := compileUpstreamTargets(servers)
	if err != nil {
		return nil, routeProtocolTerminals{}, err
	}

	if strings.EqualFold(scheme, "kafka") {
		handler, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(upstream, nil, b.getSSL)
		if err != nil {
			return nil, routeProtocolTerminals{}, err
		}
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), routeProtocolTerminals{
			kafka: routeKafkaTerminal{handler: handler},
		}, nil
	}

	// Never construct an empty round-robin picker: without nodes, target
	// selection reports a classified director error unless traffic-split
	// supplies an override for the request.
	transportOption, err := buildTransportOptionWithSSLResolver(
		r, upstream, b.getSSL, &b.staticConfig.Config,
	)
	if err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	var lb pxy.LoadBalancer
	var transport http.RoundTripper
	if len(servers) > 0 || strings.EqualFold(scheme, "grpc") || strings.EqualFold(scheme, "grpcs") {
		if b.clusterRegistry == nil {
			b.clusterRegistry = pxy.NewClusterRegistry(pxy.NopClusterObserver{})
			b.ownsClusterRegistry = true
		}
		clusterConfig, err := buildClusterConfigWithTransport(
			r,
			upstream,
			servers,
			transportOption,
			&b.staticConfig.Config,
			priorities,
		)
		if err != nil {
			return nil, routeProtocolTerminals{}, err
		}
		lease, err := b.clusterRegistry.Acquire(clusterConfig)
		if err != nil {
			return nil, routeProtocolTerminals{}, fmt.Errorf("acquire upstream cluster: %w", err)
		}
		b.addStopper(lease)
		cluster := lease.Cluster()
		lb = cluster.LoadBalancer()
		transport = cluster.RoundTripper()
	} else {
		transport = pxy.NewRetryTransport(pxy.NewTransport(transportOption))
	}
	transport = &trafficSplitRoundTripper{fallback: transport}
	director := func(req *http.Request) {
		// 1. basic
		// proxyMethod := proxyHTTP.GetMethod()
		// // support proxy method is ANY
		// if proxyMethod != methodANY {
		// 	req.Method = proxyMethod
		// }

		// 2. host: use RR/Weighted-RR to select target host
		// target is like: http://127.0.0.1 => schema + host

		originalHost := req.Host

		if applyTrafficSplitOverride(req) {
			// traffic-split selected the upstream target for this request.
		} else if lb == nil {
			*req = *withDirectorError(req, errEmptyUpstream)
			req.URL.Scheme = ""
			req.URL.Host = ""
			return
		} else if err := applyUpstreamTargetCompiled(req, lb, upstream, originalHost, compiledTargets); err != nil {
			// The reverse-proxy error handler classifies the stored
			// director error; invalidate the URL so RoundTrip cannot
			// dial the client's original host.
			*req = *withDirectorError(req, err)
			req.URL.Scheme = ""
			req.URL.Host = ""
			return
		}
		applyFinalProxyRewrite(req)

		// if u.Scheme == "" || u.Host == "" {
		// 	log.WithFields(log.Fields{"APIID": api.ID, "Stage": stage.Name, "Resource": resource.ID, "target": target}).
		// 		Error("parse host fail, invalid scheme or host")
		// 	panic("parse host fail, invalid scheme or host")
		// }

		// 3. render path

		// 4. Header: Set own default user agent. Without this line, we would get the net/http default.
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", defaultUserAgent)
		}
		markUpstreamStart(req)

		// ! later, should add target query with the req
		// ctx := context.WithValue(r.Context(), ctxRequestIDKey, requestID)
		// targetQuery := target.RawQuery
		// if targetQuery == "" || req.URL.RawQuery == "" {
		// 	req.URL.RawQuery = targetQuery + req.URL.RawQuery
		// } else {
		// 	req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		// }
	}

	modifyResponse := newModifyResponse(&b.staticConfig.Config)
	errorHandler := newErrorHandler(&b.staticConfig.Config)
	proxyHandler := pxy.NewProxyHandler(transport, director, modifyResponse, errorHandler)
	streamingProxyHandler := pxy.NewProxyHandlerWithFlushInterval(
		transport,
		director,
		modifyResponse,
		errorHandler,
		-1*time.Second,
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = pxy.WithHealthReporter(r, healthReporter(lb))
			if err := bufferRequestBodyIfNeeded(w, r); err != nil {
				ctx.SetRequestResponseSource(r, ctx.ResponseSourceAPISIX)
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					_ = util.WriteJSON(w, http.StatusRequestEntityTooLarge, err.Error())
				} else {
					_ = util.WriteJSON(w, http.StatusBadRequest, err.Error())
				}
				return
			}
			r = attachHTTPRetriesCompiled(r, upstream, lb, compiledTargets)
			selectProxyHandler(r, proxyHandler, streamingProxyHandler).ServeHTTP(w, r)
		}), routeProtocolTerminals{
			dubbo:     routeDubboTerminal{lb: lb, targets: compiledTargets, retries: httpRetryCount(upstream)},
			httpDubbo: routeHTTPDubboTerminal{lb: lb, targets: compiledTargets, retries: httpRetryCount(upstream)},
		}, nil
}

func buildKafkaPubSubProxyHandler(upstream resource.Upstream, factory kafka_proxy.KafkaConsumerFactory) http.Handler {
	handler, err := buildKafkaPubSubProxyHandlerStrict(upstream, factory)
	if err == nil {
		return handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Kafka upstream configuration invalid", http.StatusBadGateway)
	})
}

func buildKafkaPubSubProxyHandlerStrict(
	upstream resource.Upstream,
	factory kafka_proxy.KafkaConsumerFactory,
) (http.Handler, error) {
	return buildKafkaPubSubProxyHandlerStrictWithSSLResolver(upstream, factory, store.GetSSL)
}

// buildKafkaRawProxyHandler is retained for compatibility clients that speak
// raw length-prefixed Kafka frames over WebSocket. APISIX parity uses the
// PubSub handler above and never routes scheme:kafka through this extension.
func buildKafkaRawProxyHandler(lb pxy.LoadBalancer, upstream resource.Upstream) http.Handler {
	options := kafka_proxy.TransportOptions{}
	if upstream.Timeout.Connect > 0 {
		options.ConnectTimeout = time.Duration(upstream.Timeout.Connect) * time.Second
	}
	if upstream.Timeout.Send > 0 {
		options.WriteTimeout = time.Duration(upstream.Timeout.Send) * time.Second
	}
	if upstream.Timeout.Read > 0 {
		options.ReadTimeout = time.Duration(upstream.Timeout.Read) * time.Second
	}
	reporter := healthReporter(lb)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = pxy.WithHealthReporter(r, reporter)
		if !kafka_proxy.IsWebSocketUpgrade(r) {
			http.Error(w, kafka_proxy.ErrWebSocketUpgradeRequired.Error(), http.StatusUpgradeRequired)
			return
		}
		target := lb.Next()
		if target == "" {
			http.Error(w, "Kafka upstream has no configured nodes", http.StatusBadGateway)
			return
		}
		pxy.SetSelectedTarget(r, target)
		if err := kafka_proxy.ServeWebSocket(w, r, target, options); err != nil {
			pxy.ReportTCPFailureOutcome(r, false)
			if kafka_proxy.WebSocketWasHijacked(err) {
				return
			}
			http.Error(w, "Kafka upstream proxy failed", http.StatusBadGateway)
		}
	})
}
