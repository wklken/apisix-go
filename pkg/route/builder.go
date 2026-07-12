package route

import (
	"bytes"
	"context"
	"crypto/tls"
	stdjson "encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/justinas/alice"
	"github.com/unrolled/render"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
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

// FIXME: build the route incrementally in the future
// currently, we build the route in one shot
var dummyResource = []byte(`{
	"id": "123",
	"uri": "/get",
	"name": "dummy_get",
	"plugins": {
		"request_id": {"header_name": "X-Request-ID", "set_in_response": true},
		"file_logger": {"level": "info", "filename": "test.log"},
		"otel": {"server_name": "dummy_server"}
	},
	"service": {},
	"upstream": {
		"nodes": [
		{
			"host": "httpbin.org",
			"port": 80,
			"weight": 100
		}
		],
		"type": "roundrobin",
		"scheme": "http",
		"pass_host": "pass"
	}
}`)

var parameterInPathRegexp = regexp.MustCompile(`:(\w+)`)

// ConvertURI convert the apisix uri to chi compatible uri
// NOTE:
// 1. full path match: /blog/bar   same
// 2. prefix match: /blog/bar*     same
// 3. parameters in path: /blog/:name => /blog/{name} ok
// FIXME:
//
//	https://github.com/api7/lua-resty-radixtree/#parameters-in-path
//	4. not supported yet:
//	   - /user/:user/*action
//	   this will match `/user/john/` and also `/user/john/send`
//	   - /user/*action
func convertURI(uri string) (string, error) {
	// if Asterisk in the uri, and endswith it, just return
	withColon := strings.ContainsRune(uri, ':')
	withAsterisk := strings.ContainsRune(uri, '*')

	if !withColon && !withAsterisk {
		return uri, nil
	}

	if withColon && !withAsterisk {
		// replace :name with {name} in url, use regex
		uri = parameterInPathRegexp.ReplaceAllString(uri, `{$1}`)
		return uri, nil
	}

	if !withColon && withAsterisk {
		// prefix match
		if strings.HasSuffix(uri, "*") {
			return uri, nil
		}
		// not supported yet

		return "", fmt.Errorf("not supported uri: %s", uri)
	}

	if withColon && withAsterisk {
		// not supported yet
		return "", fmt.Errorf("not supported uri: %s", uri)
	}

	return "", fmt.Errorf("not supported uri: %s", uri)
}

type Builder struct {
	serverAddr string
	stoppers   []pluginStopper
	stopOnce   sync.Once
}

func NewBuilder(storage *store.Store) *Builder {
	return NewBuilderWithServerAddr(storage, "")
}

func NewBuilderWithServerAddr(storage *store.Store, serverAddr string) *Builder {
	return &Builder{serverAddr: normalizeServerAddr(serverAddr)}
}

func (b *Builder) Stop() {
	b.stopOnce.Do(func() {
		for _, stopper := range b.stoppers {
			stopper.Stop()
		}
	})
}

func (b *Builder) Build() *chi.Mux {
	if err := proxy_cache.ValidateConfiguredZones(); err != nil {
		logger.Errorf("validate proxy-cache zone registry fail: %s", err)
		return nil
	}

	routes, err := store.ListRoutes()
	if err != nil {
		logger.Errorf("list routes fail: %s", err)
		return nil
	}
	fmt.Printf("routes: %+v\n", routes)

	// routes = append(routes, dummyResource)

	mux := chi.NewRouter()

	for _, r := range routes {
		// parse route
		// methods, uris, handler, err := b.parseRouteConfig(r)
		uris := r.Uris
		if len(uris) == 0 && r.Uri != "" {
			uris = append(uris, r.Uri)
		}

		methods := r.Methods
		handler, err := b.buildHandlerStrict(r)
		if err != nil {
			logger.Errorf("build route %s fail: %s", r.ID, err)
			return nil
		}

		// if err != nil {
		// 	// log error
		// 	logger.Errorf("err: %s", err)
		// 	continue
		// }
		logger.Infof("methods: %v, uris: %v", methods, uris)
		// add route to mux
		for _, uri := range uris {
			if len(methods) == 0 {
				mux.Handle(uri, handler)
				continue
			}

			uri, err = convertURI(uri)
			if err != nil {
				logger.Warnf("convert uri fail: %w", err)
				continue
			}

			for _, method := range methods {
				if method == "PURGE" {
					logger.Warnf("http method: %s is not supported", method)
					continue
				}
				logger.Debugf("add route: %s %s", method, uri)

				mux.Method(method, uri, handler)
			}
		}
		fmt.Println("===============================")
	}

	// add extra route
	registerExtraRoutes(mux)

	return mux
}

func (b *Builder) buildHandler(r resource.Route) http.Handler {
	handler, err := b.buildHandlerStrict(r)
	if err != nil {
		logger.Errorf("build route %s fail: %s", r.ID, err)
	}
	return handler
}

func clonePluginConfigs(source map[string]resource.PluginConfig) map[string]resource.PluginConfig {
	cloned := make(map[string]resource.PluginConfig, len(source))
	for name, config := range source {
		cloned[name] = config
	}
	return cloned
}

func (b *Builder) buildHandlerStrict(r resource.Route) (http.Handler, error) {
	resourcePlugins := clonePluginConfigs(r.Plugins)
	// handle plugin_config_id
	// fmt.Println("r.Uri", r.Uri, "r.PluginConfigID", r.PluginConfigID)
	if r.PluginConfigID != "" {
		pluginConfigRule, err := store.GetPluginConfigRule(r.PluginConfigID)
		if err != nil {
			// FIXME: should return 503
			logger.Errorf("get plugin config rule fail: %s", err)
			return nil, err
		}
		for name, config := range pluginConfigRule.Plugins {
			// priority: Consumer > Route > Plugin Config > Service
			// so if not in r.Plugins, add, else skip
			if _, ok := resourcePlugins[name]; !ok {
				resourcePlugins[name] = config
			}
		}
	}

	// if service_id is not empty, get the service config
	var service resource.Service
	var err error
	if r.ServiceID != "" {
		service, err = store.GetService(r.ServiceID)
		if err != nil {
			logger.Errorf("get service fail: %s", err)
			return nil, err
		}
	}

	// add the plugins from service
	if len(service.Plugins) > 0 {
		for name, config := range service.Plugins {
			// if not in r.Plugins, add
			if _, ok := resourcePlugins[name]; !ok {
				resourcePlugins[name] = config
			}
		}
	}

	// add a context plugin, set the default vars
	systemPlugins := map[string]resource.PluginConfig{
		"request-context": buildRequestContextConfig(r, service, resourcePlugins),
	}

	var chain alice.Chain

	routeContext := b.pluginRouteContext(r)
	routeContext.service = service
	localPlugins := make([]plugin.Plugin, 0, len(resourcePlugins)+len(systemPlugins))
	initialized, err := b.initPluginsStrict(resourcePlugins, routeContext)
	if err != nil {
		return nil, err
	}
	localPlugins = append(localPlugins, initialized...)
	initialized, err = b.initPluginsStrict(systemPlugins, routeContext)
	if err != nil {
		return nil, err
	}
	localPlugins = append(localPlugins, initialized...)
	localChain := plugin.BuildPluginChain(localPlugins...)

	globalRules, err := store.ListGlobalRules()
	if err != nil {
		logger.Errorf("list global rules fail: %s", err)
		return nil, err
	}
	globalPlugins, err := b.initGlobalPluginsStrict(globalRules, routeContext)
	if err != nil {
		return nil, err
	}
	if len(globalPlugins) > 0 {
		globalChain := plugin.BuildPluginChain(globalPlugins...)

		chain = globalChain.Extend(localChain)
	} else {
		chain = localChain
	}

	handler, err := b.buildReverseHandler(r, service)
	if err != nil {
		logger.Errorf("build reverse handler fail: %s", err)
		return nil, err
	}

	return withAIExecutionTerminal(chain, handler), nil
}

func withAIExecutionTerminal(chain alice.Chain, fallback http.Handler) http.Handler {
	return ai_runtime.EnableTerminal(chain.Then(ai_runtime.TerminalHandler(fallback)))
}

func buildRequestContextConfig(
	r resource.Route,
	service resource.Service,
	pluginConfigs map[string]resource.PluginConfig,
) map[string]any {
	return map[string]any{
		"$route_id":               r.ID,
		"$route_name":             r.Name,
		"$matched_uri":            matchedURI(r),
		"$matched_host":           matchedHost(r),
		"$service_id":             r.ServiceID,
		"$service_name":           service.Name,
		"$prometheus_prefer_name": prometheusPreferName(pluginConfigs),
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

func prometheusPreferName(pluginConfigs map[string]resource.PluginConfig) bool {
	config, ok := pluginConfigs["prometheus"]
	if !ok {
		return false
	}

	values, ok := config.(map[string]any)
	if !ok {
		return false
	}

	preferName, _ := values["prefer_name"].(bool)
	return preferName
}

type pluginRouteContext struct {
	routeID    string
	serverAddr string
	route      resource.Route
	service    resource.Service
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

type pluginPrioritySetter interface {
	SetPriority(priority int)
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

func writeMetadataErrorResponse(w http.ResponseWriter, status int, value any) {
	var body []byte
	contentType := "text/plain; charset=utf-8"
	if object, ok := value.(map[string]any); ok {
		body, _ = stdjson.Marshal(object)
		contentType = "application/json"
	} else if text, ok := value.(string); ok {
		body = []byte(text)
	} else {
		body, _ = stdjson.Marshal(value)
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type pluginMetadata struct {
	disabled      bool
	priority      *int
	filter        *pluginexpr.Expression
	errorResponse any
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
	case stdjson.Number:
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

func (b *Builder) initPluginsStrict(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
) ([]plugin.Plugin, error) {
	plugins := make([]plugin.Plugin, 0, len(pluginConfigs))
	for name, config := range pluginConfigs {
		p := plugin.New(name)
		if p == nil {
			return nil, fmt.Errorf("plugin %s is not supported", name)
		}
		config, metadata, err := parsePluginMetadata(config)
		if err != nil {
			return nil, fmt.Errorf("parse plugin %s metadata: %w", name, err)
		}
		if metadata.disabled {
			continue
		}

		if err := p.Init(); err != nil {
			return nil, fmt.Errorf("initialize plugin %s: %w", name, err)
		}

		err = util.Validate(config, p.GetSchema())
		if err != nil {
			return nil, fmt.Errorf("validate plugin %s config: %w", name, err)
		}

		err = util.Parse(config, p.Config())
		if err != nil {
			return nil, fmt.Errorf("parse plugin %s config: %w", name, err)
		}

		if setter, ok := p.(pluginRouteContextSetter); ok {
			setter.SetRouteContext(routeContext.routeID, routeContext.serverAddr)
		}
		if setter, ok := p.(pluginResourceContextSetter); ok {
			setter.SetResourceContext(routeContext.route, routeContext.service)
		}
		if metadata.priority != nil {
			setter, ok := p.(pluginPrioritySetter)
			if !ok {
				return nil, fmt.Errorf("plugin %s does not support _meta.priority", name)
			}
			setter.SetPriority(*metadata.priority)
		}

		if err := p.PostInit(); err != nil {
			return nil, fmt.Errorf("initialize plugin %s: %w", name, err)
		}
		if stopper, ok := p.(pluginStopper); ok {
			b.stoppers = append(b.stoppers, stopper)
		}

		initialized := plugin.Plugin(p)
		if metadata.filter != nil || metadata.errorResponse != nil {
			initialized = metadataPlugin{
				Plugin:        p,
				filter:        metadata.filter,
				errorResponse: metadata.errorResponse,
			}
		}
		plugins = append(plugins, initialized)
	}
	return plugins, nil
}

func (b *Builder) initGlobalPlugins(
	globalRules []resource.GlobalRule,
	routeContext pluginRouteContext,
) []plugin.Plugin {
	plugins, err := b.initGlobalPluginsStrict(globalRules, routeContext)
	if err != nil {
		logger.Errorf("initialize strict global plugin set fail: %s", err)
	}
	return plugins
}

func (b *Builder) initGlobalPluginsStrict(
	globalRules []resource.GlobalRule,
	routeContext pluginRouteContext,
) ([]plugin.Plugin, error) {
	plugins := make([]plugin.Plugin, 0, len(globalRules))
	for _, rule := range globalRules {
		initialized, err := b.initPluginsStrict(rule.Plugins, routeContext)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, initialized...)
	}
	return plugins, nil
}

func (b *Builder) buildReverseHandler(r resource.Route, service resource.Service) (http.Handler, error) {
	var err error
	var upstream resource.Upstream
	// TODO: check the real priority in apisix
	// FIXME: if both upstream and upstream_id are not empty, which one should be used?
	if len(r.Upstream.Nodes) > 0 {
		upstream = r.Upstream
	} else if r.UpstreamID != "" {
		upstream, err = store.GetUpstream(r.UpstreamID)
	} else if service.Upstream.Nodes != nil {
		upstream = service.Upstream
	} else if service.UpstreamID != "" {
		upstream, err = store.GetUpstream(service.UpstreamID)
	}
	if err != nil {
		return nil, fmt.Errorf("get upstream fail: %s", err)
	}

	servers := make(map[string]int, len(upstream.Nodes))
	// fmt.Printf("the upstream nodes is: %v\n", upstream.Nodes)
	scheme := upstream.Scheme
	for _, node := range upstream.Nodes {
		host := node.Host
		port := node.Port
		weight := node.Weight

		uri := fmt.Sprintf("%s://%s:%d", scheme, host, port)
		servers[uri] = weight
	}

	if strings.EqualFold(scheme, "kafka") {
		return buildKafkaPubSubProxyHandlerStrict(upstream, nil)
	}

	// FIXME: do service discovery here
	lb, err := pxy.NewUpstreamLoadBalance(servers, upstream.Checks)
	if err != nil {
		return nil, fmt.Errorf("build upstream load balancer: %w", err)
	}

	director := func(req *http.Request) {
		// 1. basic
		// proxyMethod := proxyHTTP.GetMethod()
		// // support proxy method is ANY
		// if proxyMethod != methodANY {
		// 	req.Method = proxyMethod
		// }

		// 2. host: use RR/Weighted-RR to select target host
		// target is like: http://127.0.0.1 => schema + host

		ctx := req.Context()
		rewriteValue := ctx.Value("proxy-rewrite")
		uri := ""
		method := ""
		host := ""
		scheme := ""
		// FIXME: how to read the headers?
		if rewriteValue != nil {
			rewrite := rewriteValue.(map[string]interface{})
			uri = rewrite["uri"].(string)
			method = rewrite["method"].(string)
			host = rewrite["host"].(string)
			scheme = rewrite["scheme"].(string)
		}
		if uri != "" {
			fmt.Println("rewrite uri:", uri)
			applyProxyRewriteURI(req, uri)
		}
		if method != "" {
			req.Method = method
		}

		if host != "" {
			req.URL.Host = host
			req.Host = host
		} else if applyTrafficSplitOverride(req) {
			// traffic-split selected the upstream target for this request.
		} else {
			target := lb.Next()
			pxy.SetSelectedTarget(req, target)
			u, err := url.Parse(target)
			if err != nil {
				// log.WithFields(log.Fields{"APIID": api.ID, "Stage": stage.Name, "Resource": resource.ID, "target": target}).
				// 	Error("parse host fail, invalid target")
				// ! invalid host, just return error for the request
				panic("parse host fail, invalid target")
			}
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			req.Host = u.Host
		}

		if scheme != "" {
			req.URL.Scheme = scheme
		}

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

	//	    "timeout": {                          # Set the upstream timeout for connecting, sending and receiving messages of the route.
	//	        "connect": 3,
	//	        "send": 3,
	//	        "read": 3
	//	    },
	// 	WithResponseHeaderTimeout(responseHeaderTimeout).
	// 	Build()

	opt := (&pxy.TransportOptionBuilder{}).
		WithIdleConnTimeout(30 * time.Second).
		WithInsecureSkipVerify(true)

	// NOTE: cant set the timeout here, the openresty timeouts not match the golang timeouts
	// if r.Timeout.Connect > 0 {
	// 	connectTimeout := time.Duration(r.Timeout.Connect) * time.Second
	// 	opt = opt.WithDialTimeout(connectTimeout)
	// }

	// responseHeaderTimeout := time.Duration(timeout) * time.Second

	transport := pxy.NewTransport(opt.Build())

	modifyResponse := newModifyResponse()
	errorHandler := newErrorHandler()
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
		if serveDubboIfConfigured(w, r, lb, upstream.Retries) {
			return
		}
		if serveHTTPDubboIfConfigured(w, r, lb, upstream.Retries) {
			return
		}
		if err := bufferRequestBodyIfNeeded(r); err != nil {
			render.New().JSON(w, http.StatusBadRequest, err.Error())
			return
		}
		selectProxyHandler(r, proxyHandler, streamingProxyHandler).ServeHTTP(w, r)
	}), nil
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

type kafkaSSLResolver func(id string) (resource.SSL, error)

func buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
	upstream resource.Upstream,
	factory kafka_proxy.KafkaConsumerFactory,
	resolveSSL kafkaSSLResolver,
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
					"Kafka upstream client_cert_id cannot be combined with client_cert or client_key",
				)
			}
			id, err := normalizeKafkaSSLID(upstream.TLS.ClientCertID)
			if err != nil {
				return nil, fmt.Errorf("invalid Kafka upstream client_cert_id: %w", err)
			}
			if resolveSSL == nil {
				return nil, fmt.Errorf("Kafka upstream client_cert_id %q cannot be resolved", id)
			}
			ssl, err := resolveSSL(id)
			if err != nil {
				return nil, fmt.Errorf("resolve Kafka upstream client_cert_id %q: %w", id, err)
			}
			clientCert = ssl.Cert
			clientKey = ssl.Key
		}
		if (clientCert == "") != (clientKey == "") {
			return nil, fmt.Errorf("Kafka upstream client_cert and client_key must be configured together")
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
		brokers = append(brokers, fmt.Sprintf("kafka://%s:%d", node.Host, node.Port))
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

func normalizeKafkaSSLID(value any) (string, error) {
	switch value := value.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("must not be empty")
		}
		return value, nil
	case stdjson.Number:
		return normalizeKafkaSSLNumber(string(value))
	case float64:
		return normalizeKafkaSSLFloat(value)
	case float32:
		return normalizeKafkaSSLFloat(float64(value))
	case int:
		return strconv.Itoa(value), nil
	case int8:
		return strconv.FormatInt(int64(value), 10), nil
	case int16:
		return strconv.FormatInt(int64(value), 10), nil
	case int32:
		return strconv.FormatInt(int64(value), 10), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case uint:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint64:
		return strconv.FormatUint(value, 10), nil
	default:
		return "", fmt.Errorf("must be a string or integer")
	}
}

func normalizeKafkaSSLNumber(value string) (string, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "", fmt.Errorf("must be a string or integer")
	}
	return normalizeKafkaSSLFloat(parsed)
}

func normalizeKafkaSSLFloat(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return "", fmt.Errorf("must be an integer")
	}
	return strconv.FormatFloat(value, 'f', -1, 64), nil
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

func serveDubboIfConfigured(
	w http.ResponseWriter,
	r *http.Request,
	lb pxy.LoadBalancer,
	retries ...int,
) bool {
	cfg, ok := dubbo_proxy.GetConfig(r)
	if !ok {
		return false
	}

	retryCount := 0
	if len(retries) > 0 {
		retryCount = retries[0]
	}
	dubbo_proxy.ServeDubboWithRetries(w, r, func() (string, error) {
		return selectHTTPDubboTarget(r, lb)
	}, cfg, retryCount)
	return true
}

func serveHTTPDubboIfConfigured(
	w http.ResponseWriter,
	r *http.Request,
	lb pxy.LoadBalancer,
	retries ...int,
) bool {
	cfg, ok := http_dubbo.GetConfig(r)
	if !ok {
		return false
	}

	retryCount := 0
	if len(retries) > 0 {
		retryCount = retries[0]
	}
	http_dubbo.ServeDubboWithRetries(w, r, func() (string, error) {
		return selectHTTPDubboTarget(r, lb)
	}, cfg, retryCount)
	return true
}

func selectHTTPDubboTarget(r *http.Request, lb pxy.LoadBalancer) (string, error) {
	if override := traffic_split.GetOverride(r); override != nil {
		if override.HealthReporter != nil {
			r = pxy.WithHealthReporter(r, override.HealthReporter)
			pxy.SetSelectedTarget(r, override.HealthTarget)
		}
		return override.Host, nil
	}

	target := lb.Next()
	pxy.SetSelectedTarget(r, target)
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("parse upstream target %q: %w", target, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("upstream target %q has no host", target)
	}
	return u.Host, nil
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

func bufferRequestBodyIfNeeded(r *http.Request) error {
	if !proxy_control.GetRequestBuffering(r) || r.Body == nil || r.Body == http.NoBody {
		return nil
	}

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

func applyProxyRewriteURI(req *http.Request, uri string) {
	if parsed, err := url.ParseRequestURI(uri); err == nil && parsed.Scheme == "" && parsed.Host == "" {
		req.URL.Path = parsed.Path
		req.URL.RawPath = parsed.RawPath
		req.URL.RawQuery = parsed.RawQuery
		return
	}

	path, rawQuery, hasQuery := strings.Cut(uri, "?")
	req.URL.Path = path
	req.URL.RawPath = ""
	if hasQuery {
		req.URL.RawQuery = rawQuery
	}
}

func applyTrafficSplitOverride(req *http.Request) bool {
	override := traffic_split.GetOverride(req)
	if override == nil {
		return false
	}
	if override.HealthReporter != nil {
		req = pxy.WithHealthReporter(req, override.HealthReporter)
		pxy.SetSelectedTarget(req, override.HealthTarget)
	}
	originalHost := req.Host
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

func newModifyResponse() pxy.ModifyResponse {
	return func(resp *http.Response) error {
		// set the status into request ctx
		// ctx := resp.Request.Context()
		// ctx = context.WithValue(ctx, "status", status)

		// fmt.Println("in modify response, status:", status)

		// resp.Request = resp.Request.WithContext(ctx)

		status := resp.StatusCode
		pxy.ReportHTTPOutcome(resp.Request, status)
		if ctx.GetRequestVars(resp.Request) != nil {
			ctx.RegisterRequestVar(resp.Request, "$status", status)
			ctx.RegisterRequestVar(resp.Request, "$response_source", "upstream")
		}
		recordUpstreamLatency(resp.Request)

		// FIXME: the status here is upstream status, not the http status finally

		// FIXME: metric.UpstreamStatus
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

func newErrorHandler() pxy.ErrorHandler {
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

		// 4. check the error https://github.com/vulcand/oxy/blob/master/utils/handler.go
		status := http.StatusInternalServerError
		if !errors.Is(err, context.Canceled) {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				pxy.ReportTCPFailureOutcome(r, true)
			} else {
				pxy.ReportTCPFailureOutcome(r, false)
			}
		}
		if ctx.GetRequestVars(r) != nil {
			ctx.RegisterRequestVar(r, "$response_source", "apisix")
		}

		if e, ok := err.(net.Error); ok {
			if e.Timeout() {
				status = http.StatusGatewayTimeout
			} else {
				status = http.StatusBadGateway
			}
		} else {
			switch err {
			case io.EOF:
				status = http.StatusBadGateway
			case context.Canceled:
				status = StatusClientClosedRequest
			case io.ErrUnexpectedEOF:
				status = StatusClientClosedRequest
			}
		}

		// ! do not the raw response?
		// w.WriteHeader(statusCode)
		// ! here, not clean the body first, what will happen?
		render.New().JSON(w, status, err.Error())
	}
}
