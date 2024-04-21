package variable

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// apisix vars: https://apisix.apache.org/docs/apisix/apisix-variable/

var ApisixVars = map[string]struct{}{
	"$route_id":          {},
	"$route_name":        {},
	"$service_id":        {},
	"$service_name":      {},
	"$consumer_name":     {},
	"$consumer_group_id": {},
	"$matched_uri":       {},
}

// all apisix vars are in ctx
func GetApisixVar(r *http.Request, key string) any {
	switch key {
	case "$matched_uri":
		return chi.RouteContext(r.Context()).RoutePattern()
	default:
		return ctx.GetApisixVar(r, key)

	}
}
