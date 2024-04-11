package log

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	c "github.com/wklken/apisix-go/pkg/plugin/ctx"
)

// GetField returns the value of the field specified by key for logger.
func GetField(r *http.Request, key string) string {
	ctx := r.Context()
	switch key {
	case "method":
		return r.Method
	case "path":
		return r.URL.Path
	case "remoteIP":
		return r.RemoteAddr
	case "proto":
		return r.Proto
	case "scheme":
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case "request_id":
		return c.GetString(ctx, "request_id")
	case "matched_uri":
		return chi.RouteContext(ctx).RoutePattern()
	case "route_id":
		return c.GetString(ctx, "route_id")
	case "route_name":
		return c.GetString(ctx, "route_name")
	case "service_id":
		return c.GetString(ctx, "service_id")
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
