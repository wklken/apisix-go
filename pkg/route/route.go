package route

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-json"
	"github.com/unrolled/render"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin"
	plugin_config "github.com/wklken/apisix-go/pkg/plugin/config"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
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
	"uri": "/get",
	"name": "dummy_get",
	"plugins": {
		"request_id": {"header_name": "X-Request-ID", "set_in_response": true},
		"file_logger": {"level": "info", "filename": "test.log"}
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

func BuildRoute(routes [][]byte) *chi.Mux {
	routes = append(routes, dummyResource)

	mux := chi.NewRouter()

	for _, config := range routes {
		// parse route
		methods, uris, handler, err := parseRouteConfig(config)
		if err != nil {
			// log error
			logger.Errorf("err: %s", err)
			continue
		}
		logger.Infof("methods: %v, uris: %v", methods, uris)
		// add route to mux
		for _, uri := range uris {
			if len(methods) == 0 {
				mux.Handle(uri, handler)
				continue
			}

			for _, method := range methods {
				mux.Method(method, uri, handler)
			}
		}
	}

	return mux
}

func parseRouteConfig(config []byte) (methods []string, uris []string, handler http.Handler, err error) {
	// parse route
	// return methods, uris, handler
	var r resource.Route
	err = json.Unmarshal(config, &r)
	if err != nil {
		return
	}
	fmt.Printf("the config is: %s\n", config)

	fmt.Printf("route: %v\n", r)
	uris = r.Uris
	if len(uris) == 0 && r.Uri != "" {
		uris = append(uris, r.Uri)
	}

	methods = r.Methods
	handler = buildHandler(r)

	return
}

func buildHandler(r resource.Route) http.Handler {
	// build the route and http.Handler

	plugins := make([]plugin.Plugin, 0, len(r.Plugins))
	for name, config := range r.Plugins {
		p := plugin.New(name)
		p.Init()

		err := plugin_config.Validate(config, p.GetSchema())
		if err != nil {
			logger.Errorf("validate plugin config fail: %s", err)
			continue
		}

		err = plugin_config.Parse(config, p.Config())
		if err != nil {
			logger.Errorf("parse plugin config fail: %s", err)
			continue
		}

		logger.Infof("after parse, config: %v", p.Config())

		plugins = append(plugins, p)
	}

	// p := plugin.New("request_id")
	// p.Init(`{"header_name": "X-Request-ID", "set_in_response": true}`)

	// p1 := plugin.New("basic_auth")
	// p1.Init(`{"credentials": {"admin": "admin"}, "realm": "Restricted"}`)

	// p2 := plugin.New("file_logger")
	// p2.Init(`{"level": "info", "filename": "test.log"}`)

	// chain := plugin.BuildPluginChain(p, p1, p2)
	// chain := plugin.BuildPluginChain(p, p2)
	chain := plugin.BuildPluginChain(plugins...)
	// myHandler := http.HandlerFunc(welcomeHandler)
	handler := buildReverseHandler(r)

	return chain.Then(handler)
}

func buildReverseHandler(r resource.Route) http.Handler {
	servers := make(map[string]int, len(r.Upstream.Nodes))
	scheme := r.Upstream.Scheme
	for _, node := range r.Upstream.Nodes {
		host := node.Host
		port := node.Port
		weight := node.Weight

		uri := fmt.Sprintf("%s://%s:%d", scheme, host, port)
		servers[uri] = weight
	}
	fmt.Printf("servers: %v\n", servers)

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
		target := lb.Next()
		u, err := url.Parse(target)
		if err != nil {
			// log.WithFields(log.Fields{"APIID": api.ID, "Stage": stage.Name, "Resource": resource.ID, "target": target}).
			// 	Error("parse host fail, invalid target")
			// ! invalid host, just return error for the request
			panic("parse host fail, invalid target")
		}
		// if u.Scheme == "" || u.Host == "" {
		// 	log.WithFields(log.Fields{"APIID": api.ID, "Stage": stage.Name, "Resource": resource.ID, "target": target}).
		// 		Error("parse host fail, invalid scheme or host")
		// 	panic("parse host fail, invalid scheme or host")
		// }

		req.URL.Scheme = u.Scheme
		req.URL.Host = u.Host
		req.Host = u.Host

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
	return proxyHandler
}

func welcomeHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("welcome"))
}

func newModifyResponse() pxy.ModifyResponse {
	return func(resp *http.Response) error {
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
