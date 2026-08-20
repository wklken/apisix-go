package expr

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixvar "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
		return apisixvar.GetNginxVar(r, "$host")
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
		return base.RemoteIP(r.RemoteAddr)
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
		return HeaderValue(r.Header, header)
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

// SnapshotValue preserves RequestValue semantics after the live request has
// been detached for log and finalizer callbacks.
func SnapshotValue(snapshot base.LogSnapshot, name string) any {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "uri":
		return snapshotPath(snapshot)
	case name == "request_uri":
		return snapshot.Request.URI
	case name == "query_string" || name == "args":
		return snapshotQuery(snapshot)
	case name == "is_args":
		if snapshotQuery(snapshot) != "" {
			return "?"
		}
		return ""
	case name == "method" || name == "request_method":
		return snapshot.Request.Method
	case name == "host":
		return snapshot.Request.Host
	case name == "scheme":
		if scheme := snapshot.Request.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		return snapshot.Request.Scheme
	case name == "remote_addr":
		if value := fmt.Sprint(snapshot.Request.APISIXVars["$remote_addr"]); value != "" {
			return value
		}
		return base.RemoteIP(snapshot.Request.RemoteAddr)
	case name == "remote_port":
		if value := fmt.Sprint(snapshot.Request.APISIXVars["$remote_port"]); value != "" {
			return value
		}
		_, port, _ := net.SplitHostPort(snapshot.Request.RemoteAddr)
		return port
	case strings.HasPrefix(name, "arg_"):
		return snapshot.Request.Query.Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "cookie_"):
		request := &http.Request{Header: snapshot.Request.Header}
		cookie, err := request.Cookie(strings.TrimPrefix(name, "cookie_"))
		if err == nil {
			return cookie.Value
		}
		return ""
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return HeaderValue(snapshot.Request.Header, header)
	}
	key := "$" + name
	if value := base.LogSnapshotValue(snapshot, key); value != nil && fmt.Sprint(value) != "" {
		return value
	}
	return ""
}

func snapshotPath(snapshot base.LogSnapshot) string {
	if parsed, err := url.ParseRequestURI(snapshot.Request.URI); err == nil {
		return parsed.Path
	}
	return snapshot.Request.URI
}

func snapshotQuery(snapshot base.LogSnapshot) string {
	if parsed, err := url.ParseRequestURI(snapshot.Request.URI); err == nil {
		return parsed.RawQuery
	}
	return snapshot.Request.Query.Encode()
}

func String(value any) string {
	return stringValue(value)
}

// HeaderValue returns the single header value when exactly one exists, the
// value slice for repeated headers, and an empty string when the header is
// absent.
func HeaderValue(header http.Header, name string) any {
	values := header.Values(name)
	if len(values) == 0 {
		return ""
	}
	if len(values) == 1 {
		return values[0]
	}
	return values
}
