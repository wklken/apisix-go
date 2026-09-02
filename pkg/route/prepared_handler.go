package route

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	StatusClientClosedRequest = 499
	defaultUserAgent          = "apisix-go"
	upstreamStartTimeVar      = "$upstream_start_time"
	upstreamLatencyVar        = "$upstream_latency"
	websocketDisabledMessage  = "websocket upgrade is disabled"
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
	Consumer                  resource.Consumer
	Bindings                  []plugin.Binding
	OverrideFactories         []string
	APISIXConfigTypeSuffix    string
	APISIXConfigVersionSuffix string
}

// PreparedHandlerInput contains only owned configuration values, an already
// resolved node identity, and runtime objects whose lifecycle remains with the
// preparing generation.
type PreparedHandlerInput struct {
	Route          resource.Route
	Service        resource.Service
	StaticBindings []plugin.Binding
	Consumers      map[string]PreparedConsumerRecord
	Upstream       UpstreamPlan
	Runtime        PreparedUpstreamRuntime
	StaticConfig   appconfig.Config
	NodeID         string
	SSLs           map[string]resource.SSL
}

// BuildPreparedNotFoundHandler assembles the detached generation's 404 path
// from already materialized system/global bindings.
func BuildPreparedNotFoundHandler(
	nodeID string,
	bindings []plugin.Binding,
) (http.Handler, error) {
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
	return ensureRouteLifecycle(initializeAPISIXVars(
		responsePlan.Install(pipeline, terminal),
		nodeID,
		resource.Route{},
		resource.Service{},
	)), nil
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
	if len(terminalCandidates) == 0 {
		pipeline = pipeline.WithBeforeProxyHooksAtTransport()
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
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isWebsocketUpgradeRequest(request) {
			upgrade.ServeHTTP(writer, request)
			return
		}
		ordinary.ServeHTTP(writer, request)
	})
	return ensureRouteLifecycle(initializeAPISIXVars(
		handler,
		input.NodeID,
		routeResource,
		service,
	)), nil
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
			Consumer:                  clonePlanningConsumer(record.Consumer),
			Bindings:                  plan.StaticBindings(),
			OverrideFactories:         slices.Clone(record.OverrideFactories),
			APISIXConfigTypeSuffix:    record.APISIXConfigTypeSuffix,
			APISIXConfigVersionSuffix: record.APISIXConfigVersionSuffix,
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
		consumer = apisixctx.AttachConsumerWithSource(request, consumer, state.Source)
		overrides := make(map[string]struct{}, len(record.Bindings)+len(record.OverrideFactories))
		for _, name := range record.OverrideFactories {
			if name != "" {
				overrides[name] = struct{}{}
			}
		}
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
		request = apisixctx.WithAPISIXConfigIdentitySuffix(
			request,
			record.APISIXConfigTypeSuffix,
			record.APISIXConfigVersionSuffix,
		)
		resolution.Bindings = append([]plugin.Binding(nil), record.Bindings...)
		resolution.OverrideFactories = slices.Sorted(maps.Keys(overrides))
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
	transport := plugin.WrapBeforeProxyHooks(&trafficSplitRoundTripper{fallback: runtime.RoundTripper})
	director := func(request *http.Request) {
		applyProxyRewriteBeforeUpstream(request)
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
		request, _ := apisixctx.EnsureRequestLifecycle(r, time.Now())
		next.ServeHTTP(w, request)
	})
}

func requireWebsocketEnablement(next http.Handler, enabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !enabled && isWebsocketUpgradeRequest(r) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
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

func applyFinalProxyRewrite(request *http.Request) {
	if apisixctx.GetApisixVars(request) != nil {
		apisixctx.RegisterApisixVar(request, "$balancer_ip", request.URL.Hostname())
		apisixctx.RegisterApisixVar(request, "$balancer_port", request.URL.Port())
	}
	rewrite := apisixctx.FinalizeProxyRewrite(request)
	if rewrite.Scheme != "" {
		request.URL.Scheme = rewrite.Scheme
	}
}

func applyProxyRewriteBeforeUpstream(request *http.Request) {
	rewrite := apisixctx.FinalizeProxyRewrite(request)
	if rewrite.Host != "" {
		request.Host = rewrite.Host
	}
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
		apisixctx.SetRequestResponseSource(resp.Request, apisixctx.ResponseSourceUpstream)
		pxy.ReportHTTPOutcome(resp.Request, status)
		if apisixctx.GetRequestVars(resp.Request) != nil {
			apisixctx.RegisterRequestVar(resp.Request, "$status", status)
			if upstreamStatus == strconv.Itoa(status) {
				apisixctx.RegisterRequestVar(resp.Request, "$upstream_status", status)
			} else {
				apisixctx.RegisterRequestVar(resp.Request, "$upstream_status", upstreamStatus)
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
	if apisixctx.GetRequestVars(req) == nil {
		return
	}
	apisixctx.RegisterRequestVar(req, upstreamStartTimeVar, time.Now())
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
		if errors.As(err, &maxBytesErr) || base.IsBodyTooLarge(err) {
			apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)
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
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				pxy.ReportTCPFailureOutcome(r, true)
			} else {
				pxy.ReportTCPFailureOutcome(r, false)
			}
		}
		apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceAPISIX)

		if !overloaded {
			var netErr net.Error
			if errors.As(err, &netErr) {
				if netErr.Timeout() {
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
			if apisixctx.GetRequestVars(r) != nil {
				apisixctx.RegisterRequestVar(r, "$upstream_status", upstreamStatus)
			}
		}

		// ! do not the raw response?
		// w.WriteHeader(statusCode)
		// ! here, not clean the body first, what will happen?
		logger.Errorf("proxy request %s %s failed: %v", r.Method, proxyFailureLogPath(r), err)
		_ = util.WriteJSON(w, status, "upstream request failed")
	}
}

func recordUpstreamLatency(req *http.Request) {
	start, ok := apisixctx.GetRequestVar(req, upstreamStartTimeVar).(time.Time)
	if !ok {
		return
	}
	latency := time.Since(start).Milliseconds()
	if latency <= 0 {
		latency = 1
	}
	apisixctx.RegisterRequestVar(req, upstreamLatencyVar, latency)
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
