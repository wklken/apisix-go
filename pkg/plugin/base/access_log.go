package base

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

const accessLogVersion = "apisix-go"

var accessLogHostname = sync.OnceValue(func() string {
	hostname, _ := os.Hostname()
	return hostname
})

// AccessLogRequest is the captured request snapshot shared by the
// access-log style logger plugins.
type AccessLogRequest struct {
	Method        string
	URI           string
	URL           string
	Host          string
	ClientIP      string
	ContentLength int64
	Headers       map[string]any
	QueryString   map[string]any
	Started       time.Time
}

// CaptureMinimalAccessLogRequest captures only fields needed by custom log
// formats, avoiding header/query snapshots when the default log is disabled.
func CaptureMinimalAccessLogRequest(r *http.Request, started time.Time) AccessLogRequest {
	return AccessLogRequest{
		Host:     HostWithoutPort(r.Host),
		ClientIP: HostWithoutPort(r.RemoteAddr),
		Started:  started,
	}
}

// CaptureAccessLogRequest snapshots a request for an access-log entry.
func CaptureAccessLogRequest(r *http.Request, started time.Time, serverAddr string) AccessLogRequest {
	headers := CollapseHeaderValues(r.Header)
	headers["host"] = r.Host
	return AccessLogRequest{
		Method:        r.Method,
		URI:           r.URL.RequestURI(),
		URL:           RequestURL(r, serverAddr),
		Host:          HostWithoutPort(r.Host),
		ClientIP:      HostWithoutPort(r.RemoteAddr),
		ContentLength: max(r.ContentLength, 0),
		Headers:       headers,
		QueryString:   CollapseQueryValues(r.URL.Query()),
		Started:       started,
	}
}

// BuildAccessLogSnapshot builds the default access-log entry shared by the
// access-log style logger plugins.
func BuildAccessLogSnapshot(
	request AccessLogRequest,
	status int,
	responseHeaders http.Header,
	responseSize int64,
	routeID string,
	r *http.Request,
	duration time.Duration,
) map[string]any {
	hostname := accessLogHostname()
	latency := float64(duration) / float64(time.Millisecond)
	upstreamLatency := RequestInt64(r, "$upstream_latency")
	apisixLatency := latency - float64(upstreamLatency)
	if apisixLatency < 0 {
		apisixLatency = 0
	}
	log := map[string]any{
		"request": map[string]any{
			"url":         request.URL,
			"uri":         request.URI,
			"method":      request.Method,
			"headers":     request.Headers,
			"querystring": request.QueryString,
			"size":        request.ContentLength,
		},
		"response": map[string]any{
			"status":  status,
			"headers": CollapseHeaderValues(responseHeaders),
			"size":    responseSize,
		},
		"server": map[string]any{
			"hostname": hostname,
			"version":  accessLogVersion,
		},
		"service_id":       ApisixString(r, "$service_id"),
		"route_id":         routeID,
		"client_ip":        request.ClientIP,
		"start_time":       float64(request.Started.UnixNano()) / float64(time.Millisecond),
		"latency":          latency,
		"upstream_latency": upstreamLatency,
		"apisix_latency":   apisixLatency,
		"upstream":         UpstreamAddress(r),
	}
	if consumer := ApisixString(r, "$consumer_name"); consumer != "" {
		log["consumer"] = map[string]any{"username": consumer}
	}
	return log
}

// RequestURL reconstructs the request URL including the server address.
func RequestURL(r *http.Request, serverAddr string) string {
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := HostWithoutPort(r.Host)
	_, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		_, port, _ = net.SplitHostPort(r.Host)
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	value := scheme + "://" + authority + r.URL.RequestURI()
	return value
}

// CollapseHeaderValues normalizes header names to lowercase and collapses
// single-value headers to plain strings.
func CollapseHeaderValues(values http.Header) map[string]any {
	normalized := make(map[string][]string, len(values))
	for key, value := range values {
		key = strings.ToLower(key)
		normalized[key] = append(normalized[key], value...)
	}
	return CollapseQueryValues(normalized)
}

// CollapseQueryValues collapses single-value query parameters to plain
// strings.
func CollapseQueryValues(values map[string][]string) map[string]any {
	collapsed := make(map[string]any, len(values))
	for key, value := range values {
		if len(value) == 1 {
			collapsed[key] = value[0]
		} else {
			collapsed[key] = value
		}
	}
	return collapsed
}

// HostWithoutPort strips the port from a host or address.
func HostWithoutPort(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return strings.Trim(address, "[]")
}

// UpstreamAddress joins the balancer ip/port request variables.
func UpstreamAddress(r *http.Request) string {
	var host, port string
	if state := apisixctx.GetRequestState(r); state != nil {
		host = state.BalancerIP
		port = state.BalancerPort
	}
	if host == "" {
		host, _ = apisixctx.GetApisixVar(r, "$balancer_ip").(string)
	}
	if port == "" {
		port, _ = apisixctx.GetApisixVar(r, "$balancer_port").(string)
	}
	if host == "" || port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}

// ApisixString reads a string-valued apisix request variable.
func ApisixString(r *http.Request, key string) string {
	value, _ := apisixctx.GetApisixVar(r, key).(string)
	return value
}

// RequestInt64 reads an int-valued apisix request variable.
func RequestInt64(r *http.Request, key string) int64 {
	switch value := apisixctx.GetRequestVar(r, key).(type) {
	case int:
		return int64(value)
	case int64:
		return value
	case float64:
		return int64(value)
	default:
		return 0
	}
}
