package route

import (
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
	"github.com/unrolled/render"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
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

type Builder struct{}

func NewBuilder(storage *store.Store) *Builder {
	return &Builder{}
}

func (b *Builder) Build() *chi.Mux {
	routes, err := store.ListRoutes()
	if err != nil {
		logger.Errorf("list routes fail: %s", err)
		return nil
	}

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
	}

	// add extra route
	registerExtraRoutes(mux)

	return mux
}

func (b *Builder) buildHandler(r resource.Route) http.Handler {
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
			if _, ok := r.Plugins[name]; !ok {
				r.Plugins[name] = config
			}
		}
	}
	// FIXME: add a context plugin, set the default vars
	systemPlugins := map[string]resource.PluginConfig{
		"request-context": map[string]string{
			"$route_id":     r.ID,
			"$route_name":   r.Name,
			"$service_id":   service.ID,
			"$service_name": service.Name,
		},
	}

	plugins := make([]plugin.Plugin, 0, len(r.Plugins)+len(systemPlugins))
	for name, config := range r.Plugins {
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

		p.PostInit()

		logger.Infof("after parse, config: %v", p.Config())

		plugins = append(plugins, p)
	}

	for name, config := range systemPlugins {
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

		p.PostInit()

		logger.Infof("after parse, config: %v", p.Config())

		plugins = append(plugins, p)
	}

	chain := plugin.BuildPluginChain(plugins...)
	handler, err := b.buildReverseHandler(r, service)
	if err != nil {
		logger.Errorf("build reverse handler fail: %s", err)
		return nil
	}

	return chain.Then(handler)
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
	fmt.Printf("the upstream nodes is: %v\n", upstream.Nodes)
	scheme := upstream.Scheme
	for _, node := range upstream.Nodes {
		host := node.Host
		port := node.Port
		weight := node.Weight

		uri := fmt.Sprintf("%s://%s:%d", scheme, host, port)
		servers[uri] = weight
	}
	fmt.Printf("servers: %v\n", servers)

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
			req.URL.Path = uri
		}
		if method != "" {
			req.Method = method
		}

		if host != "" {
			req.URL.Host = host
			req.Host = host
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
	return proxyHandler, nil
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
