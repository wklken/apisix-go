package metrics

import (
	"net"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHTTPConnectionStateObserverTracksTransitionsAndTerminalCleanup(t *testing.T) {
	gauges := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_http_connections"}, []string{"state"})
	observe := newHTTPConnectionStateObserver(gauges)
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})

	observe(server, http.StateNew)
	observe(server, http.StateActive)
	if got := gaugeValue(t, gauges.WithLabelValues("new")); got != 0 {
		t.Fatalf("new connections after active transition = %v, want 0", got)
	}
	if got := gaugeValue(t, gauges.WithLabelValues("active")); got != 1 {
		t.Fatalf("active connections = %v, want 1", got)
	}

	observe(server, http.StateIdle)
	observe(server, http.StateClosed)
	observe(server, http.StateClosed)
	for _, state := range []string{"new", "active", "idle"} {
		if got := gaugeValue(t, gauges.WithLabelValues(state)); got != 0 {
			t.Fatalf("%s connections after close = %v, want 0", state, got)
		}
	}
}

func TestEtcdRuntimeMetricsSeparateReachabilityFromAppliedRevision(t *testing.T) {
	oldReachable, oldRevision := EtcdReachable, EtcdRevision
	EtcdReachable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_etcd_reachable"})
	EtcdRevision = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_etcd_revision"})
	t.Cleanup(func() { EtcdReachable, EtcdRevision = oldReachable, oldRevision })

	RecordEtcdReachable(true)
	RecordEtcdAppliedRevision(42)
	RecordEtcdReachable(false)
	RecordEtcdAppliedRevision(0)

	if got := gaugeValue(t, EtcdReachable); got != 0 {
		t.Fatalf("etcd reachable = %v, want 0", got)
	}
	if got := gaugeValue(t, EtcdRevision); got != 42 {
		t.Fatalf("etcd revision = %v, want last positive applied revision 42", got)
	}
}
