package metrics

import (
	"net"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// NewHTTPConnectionStateObserver returns one lifecycle owner suitable for
// http.Server.ConnState. Each connection contributes to exactly one bounded
// non-terminal state and is removed on close or hijack.
func NewHTTPConnectionStateObserver() func(net.Conn, http.ConnState) {
	return newHTTPConnectionStateObserver(nil)
}

func newHTTPConnectionStateObserver(injected *prometheus.GaugeVec) func(net.Conn, http.ConnState) {
	var mu sync.Mutex
	states := make(map[net.Conn]http.ConnState)
	return func(conn net.Conn, state http.ConnState) {
		if conn == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		gauges := injected
		if gauges == nil {
			gauges = Connections
		}
		if previous, ok := states[conn]; ok {
			if label, tracked := httpConnectionStateLabel(previous); tracked && gauges != nil {
				gauges.WithLabelValues(label).Dec()
			}
		}
		if label, tracked := httpConnectionStateLabel(state); tracked {
			states[conn] = state
			if gauges != nil {
				gauges.WithLabelValues(label).Inc()
			}
			return
		}
		delete(states, conn)
	}
}

func httpConnectionStateLabel(state http.ConnState) (string, bool) {
	switch state {
	case http.StateNew:
		return "new", true
	case http.StateActive:
		return "active", true
	case http.StateIdle:
		return "idle", true
	default:
		return "", false
	}
}

// RecordEtcdReachable records whether the configuration provider is currently
// reachable. Configuration acceptance remains owned by config_apply_ready.
func RecordEtcdReachable(reachable bool) {
	if EtcdReachable == nil {
		return
	}
	value := 0.0
	if reachable {
		value = 1
	}
	EtcdReachable.Set(value)
}

// RecordEtcdAppliedRevision advances the last successfully applied etcd
// revision. Invalid or empty revisions never erase a previously applied value.
func RecordEtcdAppliedRevision(revision int64) {
	if EtcdRevision == nil || revision <= 0 {
		return
	}
	EtcdRevision.Set(float64(revision))
}
