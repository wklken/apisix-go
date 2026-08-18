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
		previous, hadPrevious := states[conn]
		if hadPrevious {
			if label, tracked := httpConnectionStateLabel(previous); tracked && gauges != nil {
				gauges.WithLabelValues(label).Dec()
			}
		}
		if state == http.StateNew {
			if (!hadPrevious || previous != http.StateNew) && gauges != nil {
				gauges.WithLabelValues("accepted").Inc()
				gauges.WithLabelValues("handled").Inc()
			}
			states[conn] = state
			return
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
	case http.StateActive:
		return "active", true
	case http.StateIdle:
		return "waiting", true
	default:
		return "", false
	}
}

// RecordEtcdReachable records whether the configuration provider is currently
// reachable. Configuration acceptance remains owned by config_apply_ready.
func RecordEtcdReachable(reachable bool) {
	configApplyState.Lock()
	defer configApplyState.Unlock()

	gauge := syncEtcdReachabilityLocked()
	configApplyState.etcdObserved = true
	configApplyState.etcdHealthy = reachable
	if gauge == nil {
		return
	}
	value := 0.0
	if reachable {
		value = 1
	}
	gauge.Set(value)
}

// RecordEtcdAppliedRevision advances the last successfully applied etcd
// revision. Invalid or empty revisions never erase a previously applied value.
func RecordEtcdAppliedRevision(revision int64) {
	if revision <= 0 {
		return
	}
	if EtcdRevision != nil {
		EtcdRevision.Set(float64(revision))
	}
	if EtcdModifyIndexes != nil {
		setEtcdModifyIndex("max_modify_index", revision)
		setEtcdModifyIndex("x_etcd_index", revision)
	}
}
