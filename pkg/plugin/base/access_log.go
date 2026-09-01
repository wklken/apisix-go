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
