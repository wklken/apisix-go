package variable

import (
	"net/http"

	c "github.com/wklken/apisix-go/pkg/plugin/ctx"
)

// apisix vars: https://apisix.apache.org/docs/apisix/apisix-variable/

var ApisixVars = map[string]struct{}{
	"route_id":          {},
	"route_name":        {},
	"service_id":        {},
	"service_name":      {},
	"consumer_name":     {},
	"consumer_group_id": {},
}

// all apisix vars are in ctx
func GetApisixVar(r *http.Request, key string) string {
	return c.GetString(r.Context(), key)
}
