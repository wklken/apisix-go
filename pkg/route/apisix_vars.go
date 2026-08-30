package route

import (
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
)

func initializeAPISIXVars(
	next http.Handler,
	nodeID string,
	routeResource resource.Route,
	service resource.Service,
) http.Handler {
	values := map[string]string{
		"$node_id":      nodeID,
		"$route_id":     routeResource.ID,
		"$route_name":   routeResource.Name,
		"$matched_uri":  matchedURI(routeResource),
		"$matched_host": matchedHost(routeResource),
		"$service_id":   routeResource.ServiceID,
		"$service_name": service.Name,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = apisixctx.WithApisixVars(r, values)
		if uri, host, ok := apisixctx.MatchedRoute(r); ok {
			apisixctx.RegisterApisixVar(r, "$matched_uri", uri)
			apisixctx.RegisterApisixVar(r, "$matched_host", host)
		}
		next.ServeHTTP(w, r)
	})
}

func matchedURI(routeResource resource.Route) string {
	if routeResource.Uri != "" {
		return routeResource.Uri
	}
	if len(routeResource.Uris) > 0 {
		return routeResource.Uris[0]
	}
	return ""
}

func matchedHost(routeResource resource.Route) string {
	if len(routeResource.Hosts) > 0 {
		return routeResource.Hosts[0]
	}
	return ""
}
