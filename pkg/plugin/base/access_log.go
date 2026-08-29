package base

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
)

const accessLogVersion = "apisix-go"

var accessLogHostname = sync.OnceValue(func() string {
	hostname, _ := os.Hostname()
	return hostname
})

// Hostname returns the process hostname, cached once per process so logger
// transports never re-read it per entry.
func Hostname() string {
	return accessLogHostname()
}

// ApplyMatchedRouteFields mirrors APISIX 3.17 custom log formatting: a
// matched route overwrites the configured route_id and either overwrites or
// removes service_id according to the matched route.
func ApplyMatchedRouteFields(fields map[string]any, routeID string, serviceID string) {
	if fields == nil || routeID == "" {
		return
	}
	fields["route_id"] = routeID
	if serviceID == "" {
		delete(fields, "service_id")
		return
	}
	fields["service_id"] = serviceID
}

// ApplyRequestMatchedRouteFields resolves the matched resource identifiers
// while the live request is still available.
func ApplyRequestMatchedRouteFields(fields map[string]any, r *http.Request, fallbackRouteID string) {
	routeID := RequestVar(r, "$route_id", 0)
	if routeID == "" {
		routeID = fallbackRouteID
	}
	ApplyMatchedRouteFields(fields, routeID, RequestVar(r, "$service_id", 0))
}

// ApplySnapshotMatchedRouteFields resolves the same identifiers from the
// detached log-phase snapshot.
func ApplySnapshotMatchedRouteFields(fields map[string]any, snapshot LogSnapshot, fallbackRouteID string) {
	routeID := logIdentifier(SnapshotValue(snapshot, "$route_id"))
	if routeID == "" {
		routeID = fallbackRouteID
	}
	ApplyMatchedRouteFields(fields, routeID, logIdentifier(SnapshotValue(snapshot, "$service_id")))
}

func logIdentifier(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

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
		ClientIP: apisixctx.EffectiveRemoteIP(r),
		Started:  started,
	}
}

// CaptureAccessLogRequest snapshots a request for an access-log entry.
func CaptureAccessLogRequest(r *http.Request, started time.Time, serverAddr string) AccessLogRequest {
	headers := CollapseAccessLogHeaderValues(r.Header)
	headers["host"] = r.Host
	return AccessLogRequest{
		Method:        r.Method,
		URI:           r.URL.RequestURI(),
		URL:           RequestURL(r, serverAddr),
		Host:          HostWithoutPort(r.Host),
		ClientIP:      apisixctx.EffectiveRemoteIP(r),
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
	if r != nil {
		sensitiveQueryNames := apisixctx.SensitiveQueryNames(r)
		if len(sensitiveQueryNames) > 0 {
			request.URI = apisixlog.RedactURI(request.URI, sensitiveQueryNames)
			request.URL = apisixlog.RedactURI(request.URL, sensitiveQueryNames)
			request.QueryString = apisixlog.RedactCollapsedQuery(request.QueryString, sensitiveQueryNames)
		}
	}
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
			"headers":     collapseAccessLogHeaderMap(request.Headers),
			"querystring": request.QueryString,
			"size":        request.ContentLength,
		},
		"response": map[string]any{
			"status":  status,
			"headers": CollapseAccessLogHeaderValues(responseHeaders),
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

// BuildAccessLogFromSnapshot preserves the default access-log payload after
// the live request and response writer have been detached.
func BuildAccessLogFromSnapshot(snapshot LogSnapshot, routeID string, serverAddr ...string) map[string]any {
	duration := snapshot.Finished.Sub(snapshot.Started)
	duration = max(duration, 0)
	latency := float64(duration) / float64(time.Millisecond)
	upstreamLatency, _ := strconv.ParseFloat(fmt.Sprint(SnapshotValue(snapshot, "$upstream_latency")), 64)
	apisixLatency := max(latency-upstreamLatency, 0)
	if routeID == "" {
		routeID = fmt.Sprint(SnapshotValue(snapshot, "$route_id"))
	}
	headers := CollapseAccessLogHeaderValues(snapshot.Request.Header)
	headers["host"] = snapshot.Request.Host
	fields := map[string]any{
		"request": map[string]any{
			"url": snapshotAccessURL(snapshot, serverAddr...), "uri": snapshot.Request.URI,
			"method": snapshot.Request.Method, "headers": headers,
			"querystring": CollapseQueryValues(snapshot.Request.Query),
			"size":        max(snapshot.Request.ContentLength, 0),
		},
		"response": map[string]any{
			"status": snapshot.Outcome.Status, "headers": CollapseAccessLogHeaderValues(snapshot.Response.Header),
			"size": snapshot.Outcome.Bytes,
		},
		"server":     map[string]any{"hostname": Hostname(), "version": accessLogVersion},
		"service_id": SnapshotValue(snapshot, "$service_id"), "route_id": routeID,
		"client_ip":  fmt.Sprint(SnapshotValue(snapshot, "$remote_addr")),
		"start_time": float64(snapshot.Started.UnixNano()) / float64(time.Millisecond),
		"latency":    latency, "upstream_latency": upstreamLatency, "apisix_latency": apisixLatency,
		"upstream":        snapshotUpstreamAddress(snapshot),
		"request_id":      snapshot.Request.ID,
		"node_id":         snapshot.NodeID,
		"response_source": string(snapshot.Source),
		"outcome":         string(snapshot.Outcome.Kind),
		"upstream_status": SnapshotValue(snapshot, "$upstream_status"),
		"retry_count":     SnapshotValue(snapshot, "$retry_count"),
	}
	if consumer := snapshot.Request.Consumer.Username; consumer != "" {
		fields["consumer"] = map[string]any{"username": consumer}
	}
	return fields
}

func snapshotAccessURL(snapshot LogSnapshot, serverAddr ...string) string {
	scheme := snapshot.Request.Scheme
	if scheme == "" {
		scheme = "http"
	}
	host := HostWithoutPort(snapshot.Request.Host)
	_, port, _ := net.SplitHostPort(snapshot.Request.Host)
	if len(serverAddr) > 0 && serverAddr[0] != "" {
		if _, configuredPort, err := net.SplitHostPort(serverAddr[0]); err == nil {
			port = configuredPort
		}
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return scheme + "://" + authority + snapshot.Request.URI
}

func snapshotUpstreamAddress(snapshot LogSnapshot) string {
	if address := fmt.Sprint(SnapshotValue(snapshot, "$upstream_addr")); address != "" {
		return address
	}
	host := fmt.Sprint(SnapshotValue(snapshot, "$balancer_ip"))
	port := fmt.Sprint(SnapshotValue(snapshot, "$balancer_port"))
	if host != "" && port != "" {
		return net.JoinHostPort(host, port)
	}
	return host
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
	return collapseHeaderValues(values, nil)
}

// CollapseAccessLogHeaderValues normalizes default access-log headers while
// omitting sensitive credentials and tokens.
func CollapseAccessLogHeaderValues(values http.Header) map[string]any {
	return collapseHeaderValues(values, sensitiveAccessLogHeaders)
}

var sensitiveAccessLogHeaders = map[string]struct{}{
	"authorization":        {},
	"proxy-authorization":  {},
	"cookie":               {},
	"set-cookie":           {},
	"apikey":               {},
	"x-api-key":            {},
	"x-functions-key":      {},
	"x-amz-security-token": {},
	"x-goog-api-key":       {},
}

func collapseHeaderValues(values http.Header, omitted map[string]struct{}) map[string]any {
	normalized := make(map[string][]string, len(values))
	for key, value := range values {
		key = strings.ToLower(key)
		if _, ok := omitted[key]; ok {
			continue
		}
		normalized[key] = append(normalized[key], value...)
	}
	return CollapseQueryValues(normalized)
}

func collapseAccessLogHeaderMap(values map[string]any) map[string]any {
	collapsed := make(map[string]any, len(values))
	for key, value := range values {
		key = strings.ToLower(key)
		if _, ok := sensitiveAccessLogHeaders[key]; ok {
			continue
		}
		collapsed[key] = value
	}
	return collapsed
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
