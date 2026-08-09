package log

import (
	"net/http"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
)

// GetField returns the value of the field specified by key for logger.
func GetField(r *http.Request, key string) any {
	// not a variable
	if !strings.HasPrefix(key, "$") {
		return key
	}

	if _, ok := v.NginxVars[key]; ok {
		return v.GetNginxVar(r, key)
	}

	if _, ok := v.ApisixVars[key]; ok {
		return v.GetApisixVar(r, key)
	}

	if _, ok := v.RequestVars[key]; ok {
		return v.GetRequestVar(r, key)
	}

	// Unknown static names fall back to exact keys in the live APISIX and
	// request variable maps so runtime-registered values (for example
	// $balancer_ip, $upstream_addr, $upstream_latency) resolve without
	// growing the statically declared whitelists.
	if vars := apisixctx.GetApisixVars(r); vars != nil {
		if value, ok := vars[key]; ok {
			return value
		}
	}
	if vars := apisixctx.GetRequestVars(r); vars != nil {
		if value, ok := vars[key]; ok {
			return value
		}
	}
	return ""

	// ctx := r.Context()
	// switch key {
	// case "$matched_uri":
	// 	return chi.RouteContext(ctx).RoutePattern()
	// default:
	// 	return ""
	// }
}

func GetFields(r *http.Request, logFormat map[string]string) map[string]any {
	fields := make(map[string]any, len(logFormat))
	for key, value := range logFormat {
		if strings.HasPrefix(value, "$") {
			fields[key] = GetField(r, value)
		} else {
			fields[key] = value
		}
	}

	return fields
}
