package base

import (
	"net"
	"net/http"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

// AuthorizationResource is the safe identity subset of an APISIX resource
// that may be included in an external-authorization input.
type AuthorizationResource struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// AuthorizationFacts is the immutable request snapshot shared by external
// authorization plugins.
type AuthorizationFacts struct {
	Version    int                   `json:"version"`
	Scheme     string                `json:"scheme"`
	Method     string                `json:"method"`
	Host       string                `json:"host"`
	Path       string                `json:"path"`
	RawQuery   string                `json:"raw_query,omitempty"`
	Headers    map[string][]string   `json:"headers"`
	ClientIP   string                `json:"client_ip"`
	ClientPort string                `json:"client_port,omitempty"`
	ServerAddr string                `json:"server_addr,omitempty"`
	ServerPort string                `json:"server_port,omitempty"`
	Route      AuthorizationResource `json:"route,omitempty"`   //nolint:modernize // fixed external-authorization interface
	Service    AuthorizationResource `json:"service,omitempty"` //nolint:modernize // fixed external-authorization interface
}

// CaptureAuthorizationFacts captures only the request and safe resource
// identity fields needed by external authorization plugins.
func CaptureAuthorizationFacts(
	r *http.Request,
	serverAddr string,
	route AuthorizationResource,
	service AuthorizationResource,
) AuthorizationFacts {
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if r.URL != nil && r.URL.Scheme != "" {
		scheme = r.URL.Scheme
	}

	path, rawQuery := "", ""
	if r.URL != nil {
		path = r.URL.Path
		rawQuery = r.URL.RawQuery
	}

	headers := make(map[string][]string, len(r.Header)+1)
	for key, values := range r.Header {
		key = http.CanonicalHeaderKey(key)
		if key == "" {
			continue
		}
		headers[key] = append(headers[key], values...)
	}
	headers[http.CanonicalHeaderKey("Host")] = []string{host}

	clientIP := apisixctx.EffectiveRemoteIP(r)
	clientPort := apisixctx.GetString(r.Context(), string(apisixctx.RemotePortKey))
	if clientPort == "" {
		_, clientPort, _ = net.SplitHostPort(r.RemoteAddr)
	}

	serverHost, serverPort := serverAddr, ""
	if host, port, err := net.SplitHostPort(serverAddr); err == nil {
		serverHost, serverPort = host, port
	}

	return AuthorizationFacts{
		Version:    1,
		Scheme:     scheme,
		Method:     r.Method,
		Host:       host,
		Path:       path,
		RawQuery:   rawQuery,
		Headers:    headers,
		ClientIP:   clientIP,
		ClientPort: clientPort,
		ServerAddr: serverHost,
		ServerPort: serverPort,
		Route:      route,
		Service:    service,
	}
}
