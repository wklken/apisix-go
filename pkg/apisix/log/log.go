package log

import (
	"net/http"
	"strings"

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
	fields := make(map[string]any)
	for key, value := range logFormat {
		if strings.HasPrefix(value, "$") {
			fields[key] = GetField(r, value)
		} else {
			fields[key] = value
		}
	}

	return fields
}
