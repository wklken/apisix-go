package expr

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
)

func RequestValue(r *http.Request, name string) any {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "query_string" || name == "args":
		return r.URL.RawQuery
	case name == "is_args":
		if r.URL.RawQuery != "" {
			return "?"
		}
		return ""
	case name == "method" || name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		if value := apisixctx.GetString(r.Context(), "remote_addr"); value != "" {
			return value
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case name == "remote_port":
		if value := apisixctx.GetString(r.Context(), "remote_port"); value != "" {
			return value
		}
		_, port, _ := net.SplitHostPort(r.RemoteAddr)
		return port
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "cookie_"):
		cookie, err := r.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err == nil {
			return cookie.Value
		}
		return ""
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return requestHeaderValue(r.Header, header)
	}

	key := "$" + name
	if value := apisixvar.GetNginxVar(r, key); value != "" {
		return value
	}
	if value := apisixctx.GetApisixVar(r, key); value != nil && fmt.Sprint(value) != "" {
		return value
	}
	if value := apisixctx.GetRequestVar(r, key); value != nil {
		return value
	}
	return ""
}

func String(value any) string {
	return stringValue(value)
}

func requestHeaderValue(header http.Header, name string) any {
	values := header.Values(name)
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		return values[0]
	}
	return values
}
