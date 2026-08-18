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
	if got := gaugeValue(t, gauges.WithLabelValues("accepted")); got != 1 {
		t.Fatalf("accepted connections after new event = %v, want 1", got)
	}
	if got := gaugeValue(t, gauges.WithLabelValues("handled")); got != 1 {
		t.Fatalf("handled connections after new event = %v, want 1", got)
	}
	if got := gaugeValue(t, gauges.WithLabelValues("active")); got != 1 {
		t.Fatalf("active connections = %v, want 1", got)
	}

	observe(server, http.StateIdle)
	observe(server, http.StateClosed)
	observe(server, http.StateClosed)
	for _, state := range []string{"active", "waiting"} {
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

func TestStreamConnectionMetricsDeleteRemovedRouteSeries(t *testing.T) {
	old := StreamConnections
	streamRoutes.Lock()
	oldLimit := streamRoutes.limit
	streamRoutes.limit = 2
	streamRoutes.Unlock()
	StreamConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_stream_connections"}, []string{"route"},
	)
	registry := prometheus.NewRegistry()
	registry.MustRegister(StreamConnections)
	t.Cleanup(func() {
		SetStreamRoutes(nil)
		streamRoutes.Lock()
		streamRoutes.limit = oldLimit
		streamRoutes.Unlock()
		StreamConnections = old
	})
	SetStreamRoutes([]string{"route-c", "route-a", "route-b"})
	RecordStreamConnection("route-a")
	RecordStreamConnection("route-b")
	RecordStreamConnection("route-c")
	if got := counterVecValue(t, StreamConnections.WithLabelValues("route-a")); got != 1 {
		t.Fatalf("route-a stream count = %v, want 1", got)
	}
	if got := counterVecValue(t, StreamConnections.WithLabelValues(overflowLabel)); got != 1 {
		t.Fatalf("overflow stream count = %v, want 1", got)
	}
	SetStreamRoutes([]string{"route-b"})
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	if len(families) != 1 || len(families[0].Metric) != 1 {
		t.Fatalf("stream metric children after cleanup = %#v, want one retained child", families)
	}
	metric := families[0].Metric[0]
	if len(metric.Label) != 1 || metric.Label[0].GetName() != "route" ||
		metric.Label[0].GetValue() != "route-b" || metric.GetCounter().GetValue() != 1 {
		t.Fatalf("retained stream child = %#v, want route-b counter 1", metric)
	}
}

func TestEtcdModifyIndexesRemainMonotonicPerCategory(t *testing.T) {
	old := EtcdModifyIndexes
	EtcdModifyIndexes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_etcd_modify_indexes"}, []string{"key"},
	)
	t.Cleanup(func() { EtcdModifyIndexes = old })
	RecordEtcdModifyIndex("/apisix/routes/one", 20)
	RecordEtcdModifyIndex("/apisix/routes/two", 11)
	RecordEtcdModifyIndex("/apisix/routes/one", 15)
	if got := gaugeValue(t, EtcdModifyIndexes.WithLabelValues("routes")); got != 20 {
		t.Fatalf("routes modify index = %v, want 20", got)
	}
}

func TestGetReadinessTracksEtcdReachabilityAndCollectorReplacement(t *testing.T) {
	oldReachable := EtcdReachable
	EtcdReachable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_etcd_reachable"})
	t.Cleanup(func() { EtcdReachable = oldReachable })

	if got := GetReadiness().EtcdReachable; got {
		t.Fatal("etcd readiness = true before reachability was observed")
	}
	RecordEtcdReachable(true)
	if got := GetReadiness().EtcdReachable; !got {
		t.Fatal("etcd readiness = false after reachable observation")
	}
	RecordEtcdReachable(false)
	if got := GetReadiness().EtcdReachable; got {
		t.Fatal("etcd readiness = true after unreachable observation")
	}

	EtcdReachable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_readiness_etcd_reachable_replaced"})
	if got := GetReadiness().EtcdReachable; got {
		t.Fatal("etcd readiness = true after collector replacement")
	}
}
