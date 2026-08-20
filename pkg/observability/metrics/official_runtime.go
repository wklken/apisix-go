package metrics

import (
	"net"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var streamRoutes struct {
	sync.RWMutex
	ids      map[string]struct{}
	limit    int
	overflow bool
}

var etcdModifyState struct {
	sync.Mutex
	owner *prometheus.GaugeVec
	max   map[string]int64
}

// RecordEtcdModifyIndex records only bounded APISIX resource categories. Raw
// etcd paths are intentionally never used as label values.
func RecordEtcdModifyIndex(key string, revision int64) {
	if EtcdModifyIndexes == nil || revision <= 0 {
		return
	}
	category := etcdKeyCategory(key)
	if category == "" {
		return
	}
	setEtcdModifyIndex(category, revision)
}

func setEtcdModifyIndex(category string, revision int64) {
	etcdModifyState.Lock()
	defer etcdModifyState.Unlock()
	if etcdModifyState.owner != EtcdModifyIndexes {
		etcdModifyState.owner = EtcdModifyIndexes
		etcdModifyState.max = make(map[string]int64)
	}
	if revision <= etcdModifyState.max[category] {
		return
	}
	etcdModifyState.max[category] = revision
	EtcdModifyIndexes.WithLabelValues(category).Set(float64(revision))
}

func etcdKeyCategory(key string) string {
	for component := range strings.SplitSeq(path.Clean(key), "/") {
		switch component {
		case "routes", "services", "ssls", "consumers", "global_rules", "upstreams", "stream_routes", "protos":
			return component
		}
	}
	return ""
}

// RecordStreamConnection records one completed stream session for a matched
// route. Empty route identifiers are ignored because they are not bounded
// enough to form an official route label.
func RecordStreamConnection(route string) {
	if StreamConnections == nil || route == "" {
		return
	}
	streamRoutes.RLock()
	defer streamRoutes.RUnlock()
	_, configured := streamRoutes.ids[route]
	if configured {
		StreamConnections.WithLabelValues(route).Inc()
		return
	}
	if !streamRoutes.overflow {
		return
	}
	if httpSeriesOverflow != nil {
		httpSeriesOverflow.WithLabelValues(streamConnectionMetric).Inc()
	}
	StreamConnections.WithLabelValues(overflowLabel).Inc()
}

// SetStreamRoutes replaces the bounded route-id index used by the stream
// family. Removed routes have their children deleted so route churn cannot
// retain unbounded metric series.
func SetStreamRoutes(routeIDs []string) {
	if StreamConnections == nil {
		streamRoutes.Lock()
		streamRoutes.ids = nil
		streamRoutes.overflow = false
		streamRoutes.Unlock()
		return
	}
	ordered := append([]string(nil), routeIDs...)
	slices.Sort(ordered)
	streamRoutes.RLock()
	limit := streamRoutes.limit
	streamRoutes.RUnlock()
	if limit <= 0 {
		limit = defaultMaxMetricSeries
	}
	next := make(map[string]struct{}, min(len(ordered), limit))
	overflow := false
	previousID := ""
	for _, routeID := range ordered {
		if routeID == "" || routeID == previousID {
			continue
		}
		previousID = routeID
		if len(next) == limit {
			overflow = true
			break
		}
		next[routeID] = struct{}{}
	}
	streamRoutes.Lock()
	defer streamRoutes.Unlock()
	previous := streamRoutes.ids
	previousOverflow := streamRoutes.overflow
	streamRoutes.ids = next
	streamRoutes.overflow = overflow
	// Keep route publication and child deletion atomic with recording. If the
	// lock were released first, an in-flight completion could recreate a child
	// after its route had been retired.
	for routeID := range previous {
		if _, retained := next[routeID]; !retained {
			StreamConnections.DeleteLabelValues(routeID)
		}
	}
	if previousOverflow && !overflow {
		StreamConnections.DeleteLabelValues(overflowLabel)
	}
}

func upstreamTargetLabels(target string) (string, string) {
	parsed, err := url.Parse(target)
	if err == nil && parsed.Host != "" {
		return parsed.Hostname(), parsed.Port()
	}
	host, port, err := net.SplitHostPort(target)
	if err == nil {
		return strings.Trim(host, "[]"), port
	}
	return target, ""
}

func setUpstreamStatus(cluster, target string, healthy bool) {
	if UpstreamStatus == nil {
		return
	}
	ip, port := upstreamTargetLabels(target)
	labels := []string{cluster, ip, port}
	upstreamStatusSeries.withSeries(labels, func(actual []string) {
		value := 0.0
		if healthy {
			value = 1
		}
		UpstreamStatus.WithLabelValues(actual...).Set(value)
	})
}
