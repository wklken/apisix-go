package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	serviceName = "apisix-go"
)

// FIXME: support prefix apisix_
// FIXME: do observe in log phase
// FIXME: export the /metrics endpoint
// FIXME: how to set etcd reachable?

var (
	Connections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "http_current_connections",
			Help: "Number of HTTP connections",
		}, []string{"state"},
	)

	Requests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "http_requests_total",
			Help: "The total number of client requests since APISIX started",
		},
	)

	EtcdReachable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "etcd_reachable",
			Help: "Config server etcd reachable from APISIX, 0 is unreachable",
		},
	)

	HostInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_info",
			Help: "Info of APISIX node",
		}, []string{
			"hostname",
		},
	)

	EtcdModifyIndexed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "etcd_modify_indexes",
			Help: "Etcd modify index for APISIX keys",
		}, []string{"key"},
	)

	UpstreamStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "upstream_status",
			Help: "Upstream status from health check",
		}, []string{"name", "ip", "port"},
	)

	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_status",
			Help: "HTTP status codes per service in APISIX",
		}, []string{
			"code",
			"route",
			"matched_uri",
			"matched_host",
			"service",
			"consumer",
			"node",
			"http_status",
		},
	)

	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_latency",
			Help:    "HTTP request latency in milliseconds per service in APISIX",
			Buckets: []float64{100, 300, 1000, 5000},
		}, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
			"http_latency",
		},
	)

	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bandwidth",
			Help: "Total bandwidth in bytes consumed per service in APISIX",
		}, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
			"bandwidth",
		},
	)
)

func init() {
	prometheus.MustRegister(
		Connections,
		Requests,
		EtcdReachable,
		HostInfo,
		EtcdModifyIndexed,
		UpstreamStatus,
		HttpStatus,
		HttpLatency,
		Bandwidth,
	)
}
