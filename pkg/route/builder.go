package route

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	StatusClientClosedRequest = 499
	defaultUserAgent          = "apisix-go"
	defaultTimeout            = 300
	defaultDNSTimeout         = 5 * time.Second
	upstreamStartTimeVar      = "$upstream_start_time"
	upstreamLatencyVar        = "$upstream_latency"
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

func clonePluginConfigs(source map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	cloned := make(map[string]resource.PluginConfig, len(source))
	maps.Copy(cloned, source)
	return cloned
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

func buildTransparentUpgradeHandler(
	pipeline plugin.RequestPipeline,
	plan plugin.ResponsePlan,
	terminal http.Handler,
	enabled bool,
) (http.Handler, error) {
	terminals := plan.RouteTerminals()
	if len(terminals) == 0 {
		return pipeline.Then(requireWebsocketEnablement(terminal, enabled)), nil
	}
	streaming, err := plugin.NewStreamingResponseExecutor(nil)
	if err != nil {
		return nil, err
	}
	streaming, err = streaming.WithRouteTerminals(terminals)
	if err != nil {
		return nil, err
	}
	terminalOnly := streaming.Then(terminal)
	return pipeline.Then(requireWebsocketEnablement(terminalOnly, enabled)), nil
}

// validateRouteCompatibility is the single pre-materialization entrypoint for
// the documented Go data-plane route subset. It does not import the full
// APISIX 3.17 schema.
func validateRouteCompatibility(routeResource resource.Route) error {
	return validateRouteSemantics(routeResource)
}

func validateRouteSemantics(routeResource resource.Route) error {
	seenMethods := make(map[string]struct{}, len(routeResource.Methods))
	for _, method := range routeResource.Methods {
		if _, supported := supportedRouteMethods[method]; !supported {
			return fmt.Errorf("route %q method %q is unsupported by the Go data plane", routeResource.ID, method)
		}
		if _, duplicate := seenMethods[method]; duplicate {
			return fmt.Errorf("route %q method %q is duplicated", routeResource.ID, method)
		}
		seenMethods[method] = struct{}{}
	}
	if routeResource.HostConfigured() && routeResource.HostsConfigured() {
		return fmt.Errorf("route %q host and hosts cannot both be configured", routeResource.ID)
	}
	if routeResource.HostConfigured() && strings.TrimSpace(routeResource.Host) == "" {
		return fmt.Errorf("route %q host must not be empty", routeResource.ID)
	}
	if routeResource.HostsConfigured() && len(routeResource.EffectiveHosts()) == 0 {
		return fmt.Errorf("route %q hosts must not be empty", routeResource.ID)
	}
	for _, host := range routeResource.EffectiveHosts() {
		if err := validateRouteHost(host); err != nil {
			return fmt.Errorf("route %q host %q is invalid: %w", routeResource.ID, host, err)
		}
	}
	if routeResource.RemoteAddrConfigured() {
		return fmt.Errorf("route %q remote_addr is unsupported by the Go data plane", routeResource.ID)
	}
	if scriptID := bytes.TrimSpace(
		routeResource.ScriptID,
	); len(scriptID) > 0 &&
		!bytes.Equal(scriptID, []byte("null")) {
		return fmt.Errorf("route %q script_id is unsupported by the Go data plane", routeResource.ID)
	}
	if script := bytes.TrimSpace(routeResource.Script); len(script) > 0 && !bytes.Equal(script, []byte("null")) {
		return fmt.Errorf("route %q script is unsupported by the Go data plane", routeResource.ID)
	}
	if strings.TrimSpace(routeResource.FilterFunc) != "" {
		return fmt.Errorf("route %q filter_func is unsupported by the Go data plane", routeResource.ID)
	}
	if vars := bytes.TrimSpace(routeResource.Vars); len(vars) > 0 &&
		!bytes.Equal(vars, []byte("null")) &&
		!bytes.Equal(vars, []byte("[]")) {
		return fmt.Errorf("route %q vars is unsupported by the Go data plane", routeResource.ID)
	}
	for _, addr := range routeResource.RemoteAddrs {
		if strings.TrimSpace(addr) != "" {
			return fmt.Errorf("route %q remote_addrs is unsupported by the Go data plane", routeResource.ID)
		}
	}
	if routeResource.StatusConfigured() && routeResource.Status != 0 && routeResource.Status != 1 {
		return fmt.Errorf(
			"route %q status %d is unsupported by the Go data plane",
			routeResource.ID,
			routeResource.Status,
		)
	}
	return nil
}

func newRequestPipelineWithLog(
	bindings []plugin.Binding,
	resolve plugin.ConsumerBindingResolver,
) (plugin.RequestPipeline, error) {
	logExecutor, err := plugin.NewLogExecutorFromBindings(bindings)
	if err != nil {
		return plugin.RequestPipeline{}, err
	}
	pipeline := plugin.NewRequestPipeline(bindings, resolve)
	return pipeline.WithLogExecutor(&logExecutor), nil
}

func ensureRouteLifecycle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, _ := ctx.EnsureRequestLifecycle(r, time.Now())
		next.ServeHTTP(w, request)
	})
}

const websocketDisabledMessage = "websocket upgrade is disabled"

func requireWebsocketEnablement(next http.Handler, enabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled && isWebsocketUpgradeRequest(r) {
			ctx.SetRequestResponseSource(r, ctx.ResponseSourceAPISIX)
			_ = util.WriteJSONMessage(w, http.StatusBadRequest, websocketDisabledMessage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isWebsocketUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, value := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
			}
		}
	}
	return false
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

func pluginsFromBindings(bindings []plugin.Binding) []plugin.Plugin {
	plugins := make([]plugin.Plugin, 0, len(bindings))
	for _, binding := range bindings {
		if binding.Plugin != nil {
			plugins = append(plugins, binding.Plugin)
		}
	}
	return plugins
}

func buildSystemPluginConfigs(
	r resource.Route,
	service resource.Service,
) map[string]resource.PluginConfig {
	return map[string]resource.PluginConfig{
		"request-context": buildRequestContextConfig(r, service),
	}
}

func buildRequestContextConfig(
	r resource.Route,
	service resource.Service,
) map[string]any {
	return map[string]any{
		"$route_id":     r.ID,
		"$route_name":   r.Name,
		"$matched_uri":  matchedURI(r),
		"$matched_host": matchedHost(r),
		"$service_id":   r.ServiceID,
		"$service_name": service.Name,
	}
}

func matchedURI(r resource.Route) string {
	if r.Uri != "" {
		return r.Uri
	}
	if len(r.Uris) > 0 {
		return r.Uris[0]
	}
	return ""
}

func matchedHost(r resource.Route) string {
	if len(r.Hosts) > 0 {
		return r.Hosts[0]
	}
	return ""
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
	if phase, ok := p.(base.RequestPhasePlugin); ok &&
		requestStage != plugin.RequestStageNone && requestStage != plugin.RequestStageLegacy {
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

func resolveRouteUpstreamWithGetter(
	r resource.Route,
	service resource.Service,
	getUpstream func(string) (resource.Upstream, error),
) (resource.Upstream, plugin.ResourceProvenance, error) {
	// Keep this priority identical to buildReverseHandler: inline route,
	// route upstream_id, inline service, then service upstream_id.
	if inlineUpstreamConfigured(r.Upstream) {
		return r.Upstream, plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: r.ID}, nil
	}
	if r.UpstreamID != "" {
		upstream, err := getUpstream(r.UpstreamID)
		if err != nil {
			return resource.Upstream{}, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: r.UpstreamID},
				fmt.Errorf("get upstream %q fail: %w", r.UpstreamID, err)
		}
		return upstream, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: r.UpstreamID}, nil
	}
	if inlineUpstreamConfigured(service.Upstream) {
		return service.Upstream, plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: service.ID}, nil
	}
	if service.UpstreamID != "" {
		upstream, err := getUpstream(service.UpstreamID)
		if err != nil {
			return resource.Upstream{}, plugin.ResourceProvenance{
					Kind: plugin.ResourceUpstream,
					ID:   service.UpstreamID,
				},
				fmt.Errorf(
					"get upstream %q fail: %w",
					service.UpstreamID,
					err,
				)
		}
		return upstream, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: service.UpstreamID}, nil
	}
	return resource.Upstream{}, plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: r.ID}, nil
}

func inlineUpstreamConfigured(upstream resource.Upstream) bool {
	return upstream.Nodes != nil || upstream.Scheme != "" || upstream.TLS != nil ||
		upstream.Type != "" || upstream.Checks != nil || upstream.HashOn != "" ||
		upstream.Key != "" || upstream.PassHost != "" || upstream.UpstreamHost != "" ||
		upstream.Name != "" || upstream.Desc != "" || upstream.RetriesConfigured() ||
		upstream.Timeout != (resource.Timeout{}) || upstream.DiscoveryType != "" ||
		upstream.ServiceName != ""
}

func validateUnsupportedUpstreamDiscovery(
	upstream resource.Upstream,
	provenance plugin.ResourceProvenance,
) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "discovery_type", value: upstream.DiscoveryType},
		{name: "service_name", value: upstream.ServiceName},
	} {
		if field.value == "" {
			continue
		}
		return fmt.Errorf(
			"unsupported upstream field %q from %s %q: dynamic discovery is not supported",
			field.name,
			provenance.Kind,
			provenance.ID,
		)
	}
	return nil
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
	transportOption, err := b.buildTransportOption(r, upstream)
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

func validateHTTPUpstreamType(upstream resource.Upstream) error {
	switch strings.ToLower(upstream.Scheme) {
	case "", "http", "https", "grpc", "grpcs":
		if upstream.Type != "" && upstream.Type != "roundrobin" {
			return fmt.Errorf(
				"unsupported upstream type %q for %q scheme: only roundrobin is supported",
				upstream.Type,
				upstream.Scheme,
			)
		}
	}
	return nil
}

func upstreamNodeHost(scheme, host, port string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	standardPort := false
	switch strings.ToLower(scheme) {
	case "http", "grpc":
		standardPort = port == "80"
	case "https", "grpcs":
		standardPort = port == "443"
	}
	if standardPort {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
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

func buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
	upstream resource.Upstream,
	factory kafka_proxy.KafkaConsumerFactory,
	resolveSSL sslResolver,
) (http.Handler, error) {
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
	if upstream.TLS != nil {
		clientCert := upstream.TLS.ClientCert
		clientKey := upstream.TLS.ClientKey
		if upstream.TLS.ClientCertID != nil {
			if clientCert != "" || clientKey != "" {
				return nil, fmt.Errorf(
					"kafka upstream client_cert_id cannot be combined with client_cert or client_key",
				)
			}
			id, err := normalizeSSLID(upstream.TLS.ClientCertID)
			if err != nil {
				return nil, fmt.Errorf("invalid Kafka upstream client_cert_id: %w", err)
			}
			if resolveSSL == nil {
				return nil, fmt.Errorf("kafka upstream client_cert_id %q cannot be resolved", id)
			}
			ssl, err := resolveSSL(id)
			if err != nil {
				return nil, fmt.Errorf("resolve Kafka upstream client_cert_id %q: %w", id, err)
			}
			clientCert = ssl.Cert
			clientKey = ssl.Key
		}
		if (clientCert == "") != (clientKey == "") {
			return nil, fmt.Errorf("kafka upstream client_cert and client_key must be configured together")
		}
		tlsConfig := &tls.Config{InsecureSkipVerify: !upstream.TLS.Verify} //nolint:gosec
		if clientCert != "" {
			certificate, err := tls.X509KeyPair(
				[]byte(clientCert),
				[]byte(clientKey),
			)
			if err != nil {
				return nil, fmt.Errorf("parse Kafka upstream client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{certificate}
		}
		options.TLSConfig = tlsConfig
	}
	brokers := make([]string, 0, len(upstream.Nodes))
	for _, node := range upstream.Nodes {
		brokerHost := upstreamNodeHost("kafka", node.Host, strconv.Itoa(node.Port))
		brokers = append(brokers, "kafka://"+brokerHost)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !kafka_proxy.IsWebSocketUpgrade(r) {
			http.Error(w, kafka_proxy.ErrWebSocketUpgradeRequired.Error(), http.StatusUpgradeRequired)
			return
		}
		if len(brokers) == 0 {
			http.Error(w, "Kafka upstream has no configured nodes", http.StatusBadGateway)
			return
		}
		if err := kafka_proxy.ServePubSubWebSocket(w, r, brokers, options, factory); err != nil {
			if kafka_proxy.WebSocketWasHijacked(err) {
				return
			}
			http.Error(w, "Kafka upstream proxy failed", http.StatusBadGateway)
		}
	}), nil
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

type compiledUpstreamTarget struct {
	scheme   string
	host     string
	nodeHost string
}

func compileUpstreamTargets(servers map[string]int) (map[string]compiledUpstreamTarget, error) {
	targets := make(map[string]compiledUpstreamTarget, len(servers))
	for target := range servers {
		compiled, err := parseCompiledUpstreamTarget(target)
		if err != nil {
			return nil, err
		}
		targets[target] = compiled
	}
	return targets, nil
}

func resolveCompiledUpstreamTarget(
	target string,
	targets map[string]compiledUpstreamTarget,
) (compiledUpstreamTarget, error) {
	if compiled, ok := targets[target]; ok {
		return compiled, nil
	}
	// Compatibility fallback for direct helper callers. Built route handlers
	// always provide the immutable precompiled target table.
	return parseCompiledUpstreamTarget(target)
}

func parseCompiledUpstreamTarget(target string) (compiledUpstreamTarget, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return compiledUpstreamTarget{}, fmt.Errorf("parse upstream target %q: %w", target, err)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return compiledUpstreamTarget{}, fmt.Errorf("upstream target %q has no host", target)
	}
	port := parsed.Port()
	if port != "" {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return compiledUpstreamTarget{}, fmt.Errorf("upstream target %q has invalid port", target)
		}
	}
	return compiledUpstreamTarget{
		scheme:   parsed.Scheme,
		host:     parsed.Host,
		nodeHost: upstreamNodeHost(parsed.Scheme, parsed.Hostname(), port),
	}, nil
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

func selectProxyHandler(r *http.Request, defaultHandler http.Handler, streamingHandler http.Handler) http.Handler {
	if proxy_buffering.GetDisableProxyBuffering(r) {
		return streamingHandler
	}
	return defaultHandler
}

func healthReporter(lb pxy.LoadBalancer) pxy.HealthReporter {
	reporter, _ := lb.(pxy.HealthReporter)
	return reporter
}

type trafficSplitRoundTripper struct {
	fallback http.RoundTripper
}

func (t *trafficSplitRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if override := traffic_split.GetOverride(request); override != nil && override.RoundTripper != nil {
		return override.RoundTripper.RoundTrip(request)
	}
	return t.fallback.RoundTrip(request)
}

func bufferRequestBodyIfNeeded(w http.ResponseWriter, r *http.Request) error {
	if !proxy_control.GetRequestBuffering(r) || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	limit := proxy_control.GetRequestBufferingLimit(r)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := r.Body.Close(); err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	return nil
}

func httpRetryCount(upstream resource.Upstream) int {
	if upstream.RetriesConfigured() {
		return max(upstream.Retries, 0)
	}
	return max(len(upstream.Nodes)-1, 0)
}

func attachHTTPRetriesCompiled(
	request *http.Request,
	upstream resource.Upstream,
	loadBalancer pxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
) *http.Request {
	originalHost := request.Host
	if override := traffic_split.GetOverride(request); override != nil {
		return pxy.WithRetries(request, override.Retries, func(retry *http.Request) bool {
			if override.NextRetry == nil {
				pxy.SetSelectedTarget(retry, "")
				return false
			}
			next := override.NextRetry(retry)
			if !applyTrafficSplitTarget(retry, next, originalHost) {
				pxy.SetSelectedTarget(retry, "")
				return false
			}
			applyFinalProxyRewrite(retry)
			return true
		})
	}
	return pxy.WithRetries(request, httpRetryCount(upstream), func(retry *http.Request) bool {
		if err := applyUpstreamTargetCompiled(retry, loadBalancer, upstream, originalHost, targets); err != nil {
			return false
		}
		// A later transport failure must not report a stale director error
		// from an earlier attempt.
		*retry = *withDirectorError(retry, nil)
		applyFinalProxyRewrite(retry)
		return true
	})
}

type directorErrorContextKey struct{}

// errEmptyUpstream is reported by the director when a route has no upstream
// nodes and no traffic-split override selected a target.
var errEmptyUpstream = errors.New("upstream has no configured nodes")

func withDirectorError(request *http.Request, err error) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), directorErrorContextKey{}, err))
}

func requestDirectorError(request *http.Request) error {
	if request == nil {
		return nil
	}
	err, _ := request.Context().Value(directorErrorContextKey{}).(error)
	return err
}

func applyUpstreamTargetCompiled(
	request *http.Request,
	loadBalancer pxy.LoadBalancer,
	upstream resource.Upstream,
	originalHost string,
	targets map[string]compiledUpstreamTarget,
) error {
	target := pxy.NextTarget(loadBalancer, request)
	pxy.SetSelectedTarget(request, target)
	compiled, err := resolveCompiledUpstreamTarget(target, targets)
	if err != nil {
		return err
	}
	request.URL.Scheme = compiled.scheme
	request.URL.Host = compiled.host
	nodeHost := compiled.nodeHost
	switch upstream.PassHost {
	case "", "pass":
		request.Host = originalHost
		if request.Host == "" {
			request.Host = nodeHost
		}
	case "rewrite":
		request.Host = upstream.UpstreamHost
	case "node":
		request.Host = nodeHost
	}
	return nil
}

func applyFinalProxyRewrite(request *http.Request) {
	if ctx.GetApisixVars(request) != nil {
		ctx.RegisterApisixVar(request, "$balancer_ip", request.URL.Hostname())
		ctx.RegisterApisixVar(request, "$balancer_port", request.URL.Port())
	}
	rewrite := ctx.FinalizeProxyRewrite(request)
	if rewrite.Host != "" {
		request.Host = rewrite.Host
	}
	if rewrite.Scheme != "" {
		request.URL.Scheme = rewrite.Scheme
	}
}

func applyTrafficSplitOverride(req *http.Request) bool {
	override := traffic_split.GetOverride(req)
	return applyTrafficSplitTarget(req, override, req.Host)
}

func applyTrafficSplitTarget(req *http.Request, override *traffic_split.Override, originalHost string) bool {
	if override == nil {
		return false
	}
	if override.HealthReporter != nil {
		enriched := pxy.WithHealthReporter(req, override.HealthReporter)
		if enriched != req {
			*req = *enriched
		}
		pxy.SetSelectedTarget(req, override.HealthTarget)
	}
	req.URL.Scheme = override.Scheme
	req.URL.Host = override.Host
	switch override.PassHost {
	case "pass":
		if originalHost != "" {
			req.Host = originalHost
		} else {
			req.Host = req.URL.Host
		}
	case "rewrite":
		if override.UpstreamHost != "" {
			req.Host = override.UpstreamHost
		} else {
			req.Host = override.Host
		}
	default:
		req.Host = override.Host
	}
	return true
}

func newModifyResponse(staticConfig *appconfig.Config) pxy.ModifyResponse {
	return func(resp *http.Response) error {
		// set the status into request ctx
		// ctx := resp.Request.Context()
		// ctx = context.WithValue(ctx, "status", status)

		// resp.Request = resp.Request.WithContext(ctx)

		status := resp.StatusCode
		pxy.RecordUpstreamStatus(resp.Request, status)
		upstreamStatus := pxy.UpstreamStatusChain(resp.Request)
		if resp.Header == nil {
			resp.Header = make(http.Header)
		}
		resp.Header.Del("Server")
		resp.Header.Del("X-APISIX-Upstream-Status")
		if showUpstreamStatusInResponseHeader(staticConfig) ||
			(status >= http.StatusInternalServerError && status <= 599) {
			resp.Header.Set("X-APISIX-Upstream-Status", upstreamStatus)
		}
		ctx.SetRequestResponseSource(resp.Request, ctx.ResponseSourceUpstream)
		pxy.ReportHTTPOutcome(resp.Request, status)
		if ctx.GetRequestVars(resp.Request) != nil {
			ctx.RegisterRequestVar(resp.Request, "$status", status)
			if upstreamStatus == strconv.Itoa(status) {
				ctx.RegisterRequestVar(resp.Request, "$upstream_status", status)
			} else {
				ctx.RegisterRequestVar(resp.Request, "$upstream_status", upstreamStatus)
			}
		}
		recordUpstreamLatency(resp.Request)

		// FIXME: the status here is upstream status, not the http status finally

		// FIXME: metric.HttpLatency type=upstream

		// status := resp.StatusCode

		// req := resp.Request
		// ctx := req.Context()

		// request := resp.Request

		// // read response body and truncated
		// var body string
		// hasBody := request.Method != "HEAD" && resp.ContentLength != 0
		// if hasBody {
		// 	responseBody, err := util.ReadResponseBody(resp)
		// 	if err != nil {
		// 		body = ""
		// 	} else {
		// 		body = util.TruncateBytesToString(responseBody, 1024)
		// 	}
		// }

		// // backendPath := util.URLSingleJoiningSlash(fmt.Sprintf("%s://%s", request.URL.Scheme, request.URL.Host),
		// // 	request.URL.Path)
		// fields := log.Fields{
		// 	"backend_scheme": request.URL.Scheme,
		// 	"backend_method": request.Method,
		// 	"backend_host":   request.URL.Host,
		// 	"backend_path":   request.URL.Path,
		// 	"response_body":  body,
		// }

		// // calculate the time cost for the proxy
		// begin := request.Header.Get(middleware.TSHeader)
		// if begin != "" {
		// 	ts, err := strconv.ParseInt(begin, 10, 64)
		// 	if err == nil {
		// 		tsNow := time.Now().UnixNano() / int64(time.Millisecond)

		// 		timeCost := tsNow - ts
		// 		resp.Header.Set(timeCostRequestHeader, strconv.FormatInt(timeCost, 10))
		// 		fields["proxy_time"] = timeCost
		// 	}
		// }

		// reqctx.LogEntrySetFields(request, fields)

		return nil
	}
}

func markUpstreamStart(req *http.Request) {
	if ctx.GetRequestVars(req) == nil {
		return
	}
	ctx.RegisterRequestVar(req, upstreamStartTimeVar, time.Now())
}

func recordUpstreamLatency(req *http.Request) {
	start, ok := ctx.GetRequestVar(req, upstreamStartTimeVar).(time.Time)
	if !ok {
		return
	}
	latency := time.Since(start).Milliseconds()
	if latency <= 0 {
		latency = 1
	}
	ctx.RegisterRequestVar(req, upstreamLatencyVar, latency)
}

func newErrorHandler(staticConfig *appconfig.Config) pxy.ErrorHandler {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		// 1. make log fields
		// fields := log.Fields{
		// 	"method":     r.Method,
		// 	"uri":        r.RequestURI,
		// 	"request_id": reqctx.GetRequestID(r),
		// }
		// log.WithFields(fields).WithError(err).Error("http: proxy error")

		// // 3. set error into logging middleware
		// reqctx.LogEntrySetFields(r, log.Fields{
		// 	"error":       util.TruncateString(err.Error(), 200),
		// 	"proxy_error": "1",
		// })

		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			ctx.SetRequestResponseSource(r, ctx.ResponseSourceAPISIX)
			_ = util.WriteJSONMessage(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		// 4. check the error https://github.com/vulcand/oxy/blob/master/utils/handler.go
		status := http.StatusInternalServerError
		overloaded := errors.Is(err, pxy.ErrClusterOverloaded)
		directorFailed := false
		if overloaded {
			// The cluster is saturated. This is a capacity decision, not a
			// target failure, so it never reports a TCP health failure.
			status = http.StatusServiceUnavailable
		} else if directorErr := requestDirectorError(r); directorErr != nil {
			// The director failed target selection before RoundTrip; classify
			// it as an upstream failure instead of a client cancellation.
			err = directorErr
			status = http.StatusBadGateway
			directorFailed = true
		} else if !errors.Is(err, context.Canceled) {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				pxy.ReportTCPFailureOutcome(r, true)
			} else {
				pxy.ReportTCPFailureOutcome(r, false)
			}
		}
		ctx.SetRequestResponseSource(r, ctx.ResponseSourceAPISIX)

		if !overloaded {
			if e, ok := err.(net.Error); ok {
				if e.Timeout() {
					status = http.StatusGatewayTimeout
				} else {
					status = http.StatusBadGateway
				}
			} else {
				switch {
				case errors.Is(err, io.EOF):
					status = http.StatusBadGateway
				case errors.Is(err, context.Canceled), errors.Is(err, io.ErrUnexpectedEOF):
					status = StatusClientClosedRequest
				}
			}
		}

		w.Header().Del("X-APISIX-Upstream-Status")
		if upstreamStatus := pxy.UpstreamStatusChain(r); !directorFailed && upstreamStatus != "" {
			if showUpstreamStatusInResponseHeader(staticConfig) ||
				(status >= http.StatusInternalServerError && status <= 599) {
				w.Header().Set("X-APISIX-Upstream-Status", upstreamStatus)
			}
			if ctx.GetRequestVars(r) != nil {
				ctx.RegisterRequestVar(r, "$upstream_status", upstreamStatus)
			}
		}

		// ! do not the raw response?
		// w.WriteHeader(statusCode)
		// ! here, not clean the body first, what will happen?
		logger.Errorf("proxy request %s %s failed: %v", r.Method, proxyFailureLogPath(r), err)
		_ = util.WriteJSON(w, status, "upstream request failed")
	}
}

func showUpstreamStatusInResponseHeader(staticConfig *appconfig.Config) bool {
	return staticConfig != nil && staticConfig.Apisix.ShowUpstreamStatusInResponseHeader
}

func proxyFailureLogPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "/"
	}
	path := r.URL.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

func pinDecodedRoutePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// APISIX matches the decoded $uri; chi prefers the encoded RawPath.
		if r.URL.RawPath != "" {
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				rctx.RoutePath = r.URL.Path
			}
		}
		next.ServeHTTP(w, r)
	})
}
