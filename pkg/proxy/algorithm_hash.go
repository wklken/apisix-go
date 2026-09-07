package proxy

import (
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/expr"
)

var upstreamHashVariable = regexp.MustCompile(`\$(?:\{\s*([^}]+?)\s*\}|([\w.]+))`)

func upstreamHashValue(r *http.Request, hashOn, key string) string {
	var value any
	switch hashOn {
	case "header":
		value = upstreamHashVariableValue(r, "http_"+strings.ReplaceAll(key, "-", "_"))
	case "cookie":
		value = upstreamHashVariableValue(r, "cookie_"+key)
	case "consumer":
		value = apisixctx.GetRequestVar(r, "$consumer_name")
		if value == nil {
			value = optionalHashContextValue(r, "$consumer_name")
		}
	case "vars_combinations":
		resolved := false
		var result strings.Builder
		position := 0
		for _, match := range upstreamHashVariable.FindAllStringSubmatchIndex(key, -1) {
			if match[0] > 0 && key[match[0]-1] == '\\' {
				continue
			}
			result.WriteString(key[position:match[0]])
			start, end := match[2], match[3]
			if start < 0 {
				start, end = match[4], match[5]
			}
			name, fallback, hasFallback := strings.Cut(strings.TrimSpace(key[start:end]), "??")
			part := upstreamHashVariableValue(r, strings.TrimSpace(name))
			if part == nil && hasFallback {
				part = strings.TrimSpace(fallback)
			}
			if part != nil {
				resolved = true
				result.WriteString(expr.String(part))
			}
			position = match[1]
		}
		result.WriteString(key[position:])
		if resolved {
			value = result.String()
		}
	default:
		value = upstreamHashVariableValue(r, key)
	}
	if value == nil {
		return apisixctx.EffectiveRemoteIP(r)
	}
	return expr.String(value)
}

// Preserve missing versus present-empty values: Lua falls back only for nil.
func upstreamHashVariableValue(r *http.Request, name string) any {
	name = strings.TrimPrefix(name, "$")
	if value := apisixctx.GetRequestVar(r, "$"+name); value != nil {
		return value
	}
	switch name {
	case "server_name":
		return "_" // APISIX's HTTP server block uses server_name _.
	case "server_addr":
		if address, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			host, _, err := net.SplitHostPort(address.String())
			if err == nil {
				return host
			}
		}
		return nil
	case "hostname":
		hostname, err := os.Hostname()
		if err == nil {
			return strings.ToLower(hostname)
		}
		return nil
	}
	switch {
	case strings.HasPrefix(name, "http_"):
		header := strings.TrimPrefix(name, "http_")
		keys := make([]string, 0)
		for key := range r.Header {
			if strings.EqualFold(strings.ReplaceAll(key, "-", "_"), header) {
				keys = append(keys, key)
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			var values []string
			for _, key := range keys {
				values = append(values, r.Header[key]...)
			}
			separator := ", "
			if strings.EqualFold(header, "cookie") {
				separator = "; "
			}
			return strings.Join(values, separator)
		}
		return nil
	case strings.HasPrefix(name, "cookie_"):
		cookie, err := r.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err == nil {
			return cookie.Value
		}
		return nil
	case strings.HasPrefix(name, "arg_"):
		value, exists := expr.QueryArgument(r.URL.RawQuery, strings.TrimPrefix(name, "arg_"))
		if exists {
			return value
		}
		return nil
	}
	if _, exists := variable.NginxVars["$"+name]; exists {
		return variable.GetNginxVar(r, "$"+name)
	}
	if value := optionalHashContextValue(r, "$"+name); value != nil {
		return value
	}
	value := expr.RequestValue(r, name)
	if value == "" {
		return nil
	}
	return value
}

func optionalHashContextValue(r *http.Request, key string) any {
	value := apisixctx.GetApisixVar(r, key)
	if value == "" {
		return nil
	}
	return value
}
