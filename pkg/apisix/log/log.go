package log

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
)

// GetField returns the value of the field specified by key for logger.
func GetField(r *http.Request, key string) string {
	if _, ok := v.NginxVars[key]; ok {
		return v.GetNginxVar(r, key)
	}

	if _, ok := v.ApisixVars[key]; ok {
		return v.GetApisixVar(r, key)
	}

	ctx := r.Context()
	switch key {
	case "matched_uri":
		return chi.RouteContext(ctx).RoutePattern()
	default:
		return ""
	}
}

func GetFields(r *http.Request, keys []string) map[string]string {
	fields := make(map[string]string)
	for _, key := range keys {
		fields[key] = GetField(r, key)
	}
	return fields
}
