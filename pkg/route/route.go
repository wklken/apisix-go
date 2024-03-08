package route

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/goccy/go-json"
	"github.com/wklken/apisix-go/pkg/resource"
)

// FIXME: build the route incrementally in the future
// currently, we build the route in one shot

// {
// 	"uri": "/api/c/self-service-api/*",
// 	"name": "_bk-esb-buffet-legacy-route",
// 	"plugins": {
// 	  "proxy-rewrite": {
// 		"regex_uri": [
// 		  "/api/c/self-service-api/(.*)",
// 		  "/api/bk-esb-buffet/prod/$1"
// 		]
// 	  }
// 	},
//  "service": {
//
//  },
// 	"upstream": {
// 	  "nodes": [
// 		{
// 		  "host": "localhost",
// 		  "port": 6006,
// 		  "weight": 1
// 		}
// 	  ],
// 	  "type": "roundrobin",
// 	  "pass_host": "pass"
// 	},
// 	"labels": {
// 	  "gateway.bk.tencent.com/gateway": "-",
// 	  "gateway.bk.tencent.com/stage": "-"
// 	},
// 	"status": 1
//   }

func BuildRoute(routes [][]byte) *chi.Mux {
	mux := chi.NewRouter()

	for _, config := range routes {
		// parse route
		methods, uris, handler, err := parseRouteConfig(config)
		if err != nil {
			// log error
			continue
		}
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

	methods = r.Methods
	uris = r.Uris
	handler = buildHandler(r)

	return
}

func buildHandler(r resource.Route) http.Handler {
	// build the route and http.Handler

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// handle the request
		w.Write([]byte("hello world"))
	})
}
