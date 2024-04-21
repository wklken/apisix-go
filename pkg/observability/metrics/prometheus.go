package metrics

import (
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wklken/apisix-go/pkg/config"
)

const (
	serviceName = "apisix-go"
)

// FIXME: how to set etcd reachable?

var (
	Connections       *prometheus.GaugeVec
	Requests          prometheus.Gauge
	EtcdReachable     prometheus.Gauge
	HostInfo          *prometheus.GaugeVec
	EtcdModifyIndexed *prometheus.GaugeVec
	UpstreamStatus    *prometheus.GaugeVec
	HttpStatus        *prometheus.CounterVec
	HttpLatency       *prometheus.HistogramVec
	Bandwidth         *prometheus.CounterVec
)

func Init() {
	metricPrefix := "apisix_"
	buckets := []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}
	attr, ok := config.GlobalConfig.PluginAttr["prometheus"]
	if ok {
		if v, ok := attr["metric_prefix"]; ok {
			metricPrefix = v.(string)
		}
		if v, ok := attr["default_buckets"]; ok {
			// FIXME: maybe bug here, the unmarshal maybe wrong
			buckets = v.([]float64)
		}
	}

	// FIXME
	Connections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricPrefix + "http_current_connections",
			Help: "Number of HTTP connections",
		}, []string{"state"},
	)

	// pkg/plugin/request_context/plugin.go
	Requests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricPrefix + "http_requests_total",
			Help: "The total number of client requests since APISIX started",
		},
	)

	// FIXME
	EtcdReachable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricPrefix + "etcd_reachable",
			Help: "Config server etcd reachable from APISIX, 0 is unreachable",
		},
	)

	HostInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricPrefix + "node_info",
			Help: "Info of APISIX node",
		}, []string{
			"hostname",
		},
	)

	// FIXME
	EtcdModifyIndexed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricPrefix + "etcd_modify_indexes",
			Help: "Etcd modify index for APISIX keys",
		}, []string{"key"},
	)

	// FIXME
	UpstreamStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricPrefix + "upstream_status",
			Help: "Upstream status from health check",
		}, []string{"name", "ip", "port"},
	)

	// pkg/plugin/request_context/plugin.go
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "http_status",
			Help: "HTTP status codes per service in APISIX",
		}, []string{
			"code",
			"route",
			"matched_uri",
			"matched_host",
			"service",
			"consumer",
			"node",
		},
	)

	// type = request: pkg/plugin/request_context/plugin.go
	// FIXME: type = apisix:
	// FIXME: type = upstream:
	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricPrefix + "http_latency",
			Help:    "HTTP request latency in milliseconds per service in APISIX",
			Buckets: buckets,
		}, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
		},
	)

	// type = ingress: pkg/plugin/request_context/plugin.go
	// FIXME: type = egress
	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricPrefix + "bandwidth",
			Help: "Total bandwidth in bytes consumed per service in APISIX",
		}, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
		},
	)

	hostName := "unknown"
	hostName, _ = os.Hostname()
	HostInfo.WithLabelValues(hostName).Set(1)

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
