package route

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/justinas/alice"
	"github.com/unrolled/render"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
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
}

func NewBuilder(storage *store.Store) *Builder {
	return NewBuilderWithServerAddr(storage, "")
}

func NewBuilderWithServerAddr(storage *store.Store, serverAddr string) *Builder {
	return &Builder{serverAddr: normalizeServerAddr(serverAddr)}
}

func (b *Builder) Build() *chi.Mux {
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
		handler := b.buildHandler(r)

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
	resourcePlugins := r.Plugins
	// handle plugin_config_id
	// fmt.Println("r.Uri", r.Uri, "r.PluginConfigID", r.PluginConfigID)
	if r.PluginConfigID != "" {
		pluginConfigRule, err := store.GetPluginConfigRule(r.PluginConfigID)
		if err != nil {
			// FIXME: should return 503
			logger.Errorf("get plugin config rule fail: %s", err)
			return nil
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
			return nil
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
	localPlugins := make([]plugin.Plugin, 0, len(resourcePlugins)+len(systemPlugins))
	localPlugins = append(localPlugins, b.initPlugins(resourcePlugins, routeContext)...)
	localPlugins = append(localPlugins, b.initPlugins(systemPlugins, routeContext)...)
	localChain := plugin.BuildPluginChain(localPlugins...)

	globalRules, err := store.ListGlobalRules()
	if err != nil {
		logger.Errorf("list global rules fail: %s", err)
		return nil
	}
	globalPlugins := b.initGlobalPlugins(globalRules, routeContext)
	if len(globalPlugins) > 0 {
		globalChain := plugin.BuildPluginChain(globalPlugins...)

		chain = globalChain.Extend(localChain)
	} else {
		chain = localChain
	}

	handler, err := b.buildReverseHandler(r, service)
	if err != nil {
		logger.Errorf("build reverse handler fail: %s", err)
		return nil
	}

	return chain.Then(handler)
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
}

func (b *Builder) pluginRouteContext(r resource.Route) pluginRouteContext {
	return pluginRouteContext{
		routeID:    r.ID,
		serverAddr: b.serverAddr,
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

func (b *Builder) initPlugins(
	pluginConfigs map[string]resource.PluginConfig,
	routeContext pluginRouteContext,
) []plugin.Plugin {
	plugins := make([]plugin.Plugin, 0, len(pluginConfigs))
	for name, config := range pluginConfigs {
		p := plugin.New(name)
		if p == nil {
			logger.Warnf("plugin %s not supported yet", name)
			continue
		}
		p.Init()

		err := util.Validate(config, p.GetSchema())
		if err != nil {
			logger.Errorf("validate plugin %s config fail: %s", name, err)
			continue
		}

		err = util.Parse(config, p.Config())
		if err != nil {
			logger.Errorf("parse plugin config fail: %s", err)
			continue
		}

		if setter, ok := p.(pluginRouteContextSetter); ok {
			setter.SetRouteContext(routeContext.routeID, routeContext.serverAddr)
		}

		p.PostInit()

		plugins = append(plugins, p)
	}
	return plugins
}

func (b *Builder) initGlobalPlugins(
	globalRules []resource.GlobalRule,
	routeContext pluginRouteContext,
) []plugin.Plugin {
	plugins := make([]plugin.Plugin, 0, len(globalRules))
	for _, rule := range globalRules {
		plugins = append(plugins, b.initPlugins(rule.Plugins, routeContext)...)
	}
	return plugins
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

	// FIXME: do service discovery here
	lb := pxy.NewWeightedRRLoadBalance(servers)

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
		if serveHTTPDubboIfConfigured(w, r, lb) {
			return
		}
		if err := bufferRequestBodyIfNeeded(r); err != nil {
			render.New().JSON(w, http.StatusBadRequest, err.Error())
			return
		}
		selectProxyHandler(r, proxyHandler, streamingProxyHandler).ServeHTTP(w, r)
	}), nil
}

func serveHTTPDubboIfConfigured(w http.ResponseWriter, r *http.Request, lb pxy.LoadBalancer) bool {
	cfg, ok := http_dubbo.GetConfig(r)
	if !ok {
		return false
	}

	target, err := selectHTTPDubboTarget(r, lb)
	if err != nil {
		render.New().JSON(w, http.StatusBadGateway, err.Error())
		return true
	}
	http_dubbo.ServeDubbo(w, r, target, cfg)
	return true
}

func selectHTTPDubboTarget(r *http.Request, lb pxy.LoadBalancer) (string, error) {
	if override := traffic_split.GetOverride(r); override != nil {
		return override.Host, nil
	}

	target := lb.Next()
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
	path, rawQuery, hasQuery := strings.Cut(uri, "?")
	req.URL.Path = path
	if hasQuery {
		req.URL.RawQuery = rawQuery
	}
}

func applyTrafficSplitOverride(req *http.Request) bool {
	override := traffic_split.GetOverride(req)
	if override == nil {
		return false
	}
	req.URL.Scheme = override.Scheme
	req.URL.Host = override.Host
	req.Host = override.Host
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
		ctx.RegisterRequestVar(resp.Request, "$status", status)
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
