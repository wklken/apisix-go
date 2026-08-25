package route

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

// PreparedUpstreamRuntime is the authority-free view of an already prepared
// upstream. It intentionally exposes neither the cluster nor its Close method.
type PreparedUpstreamRuntime struct {
	LoadBalancer pxy.LoadBalancer
	RoundTripper http.RoundTripper
}

// PreparedConsumerRecord is one generation-owned consumer resolution result.
// Bindings are materialized before route assembly; Consumer is the trusted
// value attached after authentication selects this record.
type PreparedConsumerRecord struct {
	Consumer resource.Consumer
	Bindings []plugin.Binding
}

// PreparedHandlerInput contains only owned configuration values and runtime
// objects whose lifecycle remains with the preparing generation.
type PreparedHandlerInput struct {
	Route          resource.Route
	Service        resource.Service
	StaticBindings []plugin.Binding
	Consumers      map[string]PreparedConsumerRecord
	Upstream       UpstreamPlan
	Runtime        PreparedUpstreamRuntime
	StaticConfig   appconfig.Config
	SSLs           map[string]resource.SSL
}

// BuildPreparedNotFoundHandler assembles the detached generation's 404 path
// from already materialized system/global bindings.
func BuildPreparedNotFoundHandler(bindings []plugin.Binding) (http.Handler, error) {
	for _, binding := range bindings {
		if binding.Scope != plugin.ScopeSystem && binding.Scope != plugin.ScopeGlobal {
			return nil, fmt.Errorf(
				"build prepared not-found handler: binding %q has unsupported scope %d",
				binding.Descriptor.Factory,
				binding.Scope,
			)
		}
	}
	responsePlan, err := plugin.BuildResponsePlan(plugin.ResponsePlanInput{
		StaticBindings: bindings,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		return nil, fmt.Errorf("build prepared not-found handler response plan: %w", err)
	}
	pipeline, err := newRequestPipelineWithLog(bindings, nil)
	if err != nil {
		return nil, fmt.Errorf("build prepared not-found handler request pipeline: %w", err)
	}
	terminal := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceEarlyStop)
		http.NotFoundHandler().ServeHTTP(writer, request)
	})
	return ensureRouteLifecycle(responsePlan.Install(pipeline, terminal)), nil
}

// BuildPreparedHandler assembles one detached route handler without Store
// reads, plugin construction, resource acquisition, or lifecycle ownership.
func BuildPreparedHandler(input PreparedHandlerInput) (http.Handler, error) {
	routeResource := cloneCompileRoute(input.Route)
	service := clonePlanningService(input.Service)
	upstream := clonePlannedUpstream(input.Upstream.Upstream)
	targets := clonePreparedTargets(input.Upstream.Targets)

	if routeResource.ID == "" {
		return nil, fmt.Errorf("build prepared handler: route id is required")
	}
	if err := validateRouteCompatibility(routeResource); err != nil {
		return nil, fmt.Errorf("build prepared handler route %q: %w", routeResource.ID, err)
	}
	if !strings.EqualFold(upstream.Scheme, "kafka") && input.Runtime.RoundTripper == nil {
		return nil, fmt.Errorf("build prepared handler route %q: upstream round tripper is required", routeResource.ID)
	}
	consumerResolver, err := freezePreparedConsumerResolver(
		input.Consumers,
		routeResource,
		service,
	)
	if err != nil {
		return nil, fmt.Errorf("build prepared handler route %q consumers: %w", routeResource.ID, err)
	}

	terminal, terminals, err := buildPreparedReverseHandler(
		routeResource,
		upstream,
		targets,
		input.Runtime,
		preparedStaticConfig(input.StaticConfig),
		clonePreparedSSLs(input.SSLs),
	)
	if err != nil {
		return nil, fmt.Errorf("build prepared handler route %q: %w", routeResource.ID, err)
	}
	terminalCandidates := preparedRouteTerminalCandidates(
		input.StaticBindings,
		input.Upstream.Provenance,
		upstream,
		terminals,
	)
	responsePlan, err := plugin.BuildResponsePlan(plugin.ResponsePlanInput{
		StaticBindings: input.StaticBindings,
		RouteTerminals: terminalCandidates,
		BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
	})
	if err != nil {
		return nil, fmt.Errorf("build prepared handler route %q response plan: %w", routeResource.ID, err)
	}
	pipeline, err := newRequestPipelineWithLog(input.StaticBindings, consumerResolver)
	if err != nil {
		return nil, fmt.Errorf("build prepared handler route %q request pipeline: %w", routeResource.ID, err)
	}
	websocketEnabled := routeResource.EnableWebsocket
	if !routeResource.EnableWebsocketConfigured() {
		websocketEnabled = service.EnableWebsocket
	}
	ordinary := responsePlan.Install(pipeline, requireWebsocketEnablement(terminal, websocketEnabled))
	upgrade, err := buildTransparentUpgradeHandler(pipeline, responsePlan, terminal, websocketEnabled)
	if err != nil {
		return nil, fmt.Errorf("build prepared handler route %q upgrade path: %w", routeResource.ID, err)
	}
	return ensureRouteLifecycle(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isWebsocketUpgradeRequest(request) {
			upgrade.ServeHTTP(writer, request)
			return
		}
		ordinary.ServeHTTP(writer, request)
	})), nil
}

func freezePreparedConsumerResolver(
	supplied map[string]PreparedConsumerRecord,
	routeResource resource.Route,
	service resource.Service,
) (plugin.ConsumerBindingResolver, error) {
	records := make(map[string]PreparedConsumerRecord, len(supplied))
	for _, identity := range slices.Sorted(maps.Keys(supplied)) {
		if identity == "" {
			return nil, fmt.Errorf("consumer identity is required")
		}
		record := supplied[identity]
		if record.Consumer.Username == "" {
			return nil, fmt.Errorf("consumer %q username is required", identity)
		}
		plan, err := plugin.BuildResponsePlan(record.Bindings)
		if err != nil {
			return nil, fmt.Errorf("consumer %q bindings: %w", identity, err)
		}
		records[identity] = PreparedConsumerRecord{
			Consumer: clonePlanningConsumer(record.Consumer),
			Bindings: plan.StaticBindings(),
		}
	}
	serviceID := service.ID
	if serviceID == "" {
		serviceID = routeResource.ServiceID
	}
	return func(request *http.Request) (plugin.ConsumerResolution, error) {
		resolution := plugin.ConsumerResolution{Request: request}
		state, authenticated := apisixctx.AuthenticationStateFrom(request)
		if !authenticated {
			return resolution, nil
		}
		authenticatedConsumer := state.Consumer()
		record, prepared := records[authenticatedConsumer.Username]
		if !prepared {
			return resolution, fmt.Errorf(
				"authenticated consumer %q from %q is absent from prepared generation",
				authenticatedConsumer.Username,
				state.Source,
			)
		}
		consumer := clonePlanningConsumer(record.Consumer)
		request = apisixctx.WithApisixVars(request, nil)
		apisixctx.AttachConsumer(request, consumer)
		overrides := make(map[string]struct{}, len(record.Bindings))
		for _, binding := range record.Bindings {
			name := binding.Descriptor.Factory
			if name == "" && binding.Plugin != nil {
				name = binding.Plugin.GetName()
			}
			if name != "" {
				overrides[name] = struct{}{}
			}
		}
		request = apisixctx.WithConsumerPluginOverrides(request, overrides)
		resolution.Bindings = append([]plugin.Binding(nil), record.Bindings...)
		resolution.Request = request
		resolution.CacheKey = plugin.ConsumerCacheKey{
			ConsumerID: consumer.Username,
			GroupID:    consumer.GroupID,
			RouteID:    routeResource.ID,
			ServiceID:  serviceID,
		}
		resolution.Identity = plugin.ConsumerIdentity{
			Username:   consumer.Username,
			GroupID:    consumer.GroupID,
			AuthSource: state.Source,
		}
		resolution.Resolved = true
		return resolution, nil
	}, nil
}

func clonePreparedTargets(source map[string]int) map[string]int {
	if source == nil {
		return nil
	}
	return maps.Clone(source)
}

func clonePreparedSSLs(source map[string]resource.SSL) map[string]resource.SSL {
	if source == nil {
		return nil
	}
	cloned := make(map[string]resource.SSL, len(source))
	for id, ssl := range source {
		cloned[id] = clonePlannedSSL(ssl)
	}
	return cloned
}

func preparedStaticConfig(source appconfig.Config) *appconfig.Config {
	return &appconfig.Config{Apisix: appconfig.Apisix{
		ShowUpstreamStatusInResponseHeader: source.Apisix.ShowUpstreamStatusInResponseHeader,
	}}
}

func buildPreparedReverseHandler(
	routeResource resource.Route,
	upstream resource.Upstream,
	targets map[string]int,
	runtime PreparedUpstreamRuntime,
	staticConfig *appconfig.Config,
	ssls map[string]resource.SSL,
) (http.Handler, routeProtocolTerminals, error) {
	if strings.EqualFold(upstream.Scheme, "kafka") {
		handler, err := buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
			upstream,
			nil,
			plannedSSLResolver(ssls),
		)
		if err != nil {
			return nil, routeProtocolTerminals{}, err
		}
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), routeProtocolTerminals{
			kafka: routeKafkaTerminal{handler: handler},
		}, nil
	}
	compiledTargets, err := compileUpstreamTargets(targets)
	if err != nil {
		return nil, routeProtocolTerminals{}, err
	}
	loadBalancer := runtime.LoadBalancer
	transport := &trafficSplitRoundTripper{fallback: runtime.RoundTripper}
	director := func(request *http.Request) {
		originalHost := request.Host
		if applyTrafficSplitOverride(request) {
			// The materialized traffic-split binding selected a prepared runtime.
		} else if loadBalancer == nil {
			*request = *withDirectorError(request, errEmptyUpstream)
			request.URL.Scheme = ""
			request.URL.Host = ""
			return
		} else if err := applyUpstreamTargetCompiled(
			request,
			loadBalancer,
			upstream,
			originalHost,
			compiledTargets,
		); err != nil {
			*request = *withDirectorError(request, err)
			request.URL.Scheme = ""
			request.URL.Host = ""
			return
		}
		applyFinalProxyRewrite(request)
		if _, configured := request.Header["User-Agent"]; !configured {
			request.Header.Set("User-Agent", defaultUserAgent)
		}
		markUpstreamStart(request)
	}
	modifyResponse := newModifyResponse(staticConfig)
	errorHandler := newErrorHandler(staticConfig)
	proxyHandler := pxy.NewProxyHandler(transport, director, modifyResponse, errorHandler)
	streamingProxyHandler := pxy.NewProxyHandlerWithFlushInterval(
		transport,
		director,
		modifyResponse,
		errorHandler,
		-time.Second,
	)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			request = pxy.WithHealthReporter(request, healthReporter(loadBalancer))
			if err := bufferRequestBodyIfNeeded(writer, request); err != nil {
				apisixctx.SetRequestResponseSource(request, apisixctx.ResponseSourceAPISIX)
				var maxBytesErr *http.MaxBytesError
				if errors.As(err, &maxBytesErr) {
					_ = util.WriteJSON(writer, http.StatusRequestEntityTooLarge, err.Error())
				} else {
					_ = util.WriteJSON(writer, http.StatusBadRequest, err.Error())
				}
				return
			}
			request = attachHTTPRetriesCompiled(request, upstream, loadBalancer, compiledTargets)
			selectProxyHandler(request, proxyHandler, streamingProxyHandler).ServeHTTP(writer, request)
		}), routeProtocolTerminals{
			dubbo: routeDubboTerminal{
				lb: loadBalancer, targets: compiledTargets, retries: httpRetryCount(upstream),
			},
			httpDubbo: routeHTTPDubboTerminal{
				lb: loadBalancer, targets: compiledTargets, retries: httpRetryCount(upstream),
			},
		}, nil
}

func preparedRouteTerminalCandidates(
	bindings []plugin.Binding,
	upstreamProvenance plugin.ResourceProvenance,
	upstream resource.Upstream,
	terminals routeProtocolTerminals,
) []plugin.RouteTerminalCandidate {
	candidates := make([]plugin.RouteTerminalCandidate, 0, 1)
	seen := make(map[plugin.ProtocolKind]bool)
	for _, binding := range bindings {
		var protocol plugin.ProtocolKind
		var terminal base.ExclusiveProtocolTerminal
		switch binding.Descriptor.Factory {
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
			Identity:   binding.Descriptor.Factory,
			Scope:      binding.Scope,
			Priority:   binding.Priority,
			Provenance: binding.Provenance,
			Protocol:   protocol,
			Terminal:   terminal,
		})
	}
	if strings.EqualFold(upstream.Scheme, "kafka") && terminals.kafka != nil && !seen[plugin.ProtocolKafka] {
		candidates = append(candidates, plugin.RouteTerminalCandidate{
			Identity:   "kafka-proxy",
			Scope:      plugin.ScopeRoute,
			Provenance: upstreamProvenance,
			Protocol:   plugin.ProtocolKafka,
			Terminal:   terminals.kafka,
		})
	}
	return candidates
}
