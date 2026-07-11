package metrics

import (
	"fmt"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cast"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
)

const (
	serviceName = "apisix-go"
)

var defaultLatencyBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

// FIXME: how to set etcd reachable?

var (
	Connections          *prometheus.GaugeVec
	Requests             prometheus.Gauge
	EtcdReachable        prometheus.Gauge
	HostInfo             *prometheus.GaugeVec
	EtcdModifyIndexed    *prometheus.GaugeVec
	UpstreamStatus       *prometheus.GaugeVec
	HttpStatus           *prometheus.CounterVec
	HttpLatency          *prometheus.HistogramVec
	Bandwidth            *prometheus.CounterVec
	BatchProcessEntries  *prometheus.GaugeVec
	LLMActiveConnections *prometheus.GaugeVec
)

func Init() {
	var attr map[string]interface{}
	if config.GlobalConfig != nil {
		attr = config.GlobalConfig.PluginAttr["prometheus"]
	}
	metricConfig := newPrometheusMetricConfig(attr)

	// FIXME
	Connections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "http_current_connections",
			Help: "Number of HTTP connections",
		}, []string{"state"},
	)

	// pkg/plugin/request_context/plugin.go
	Requests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "http_requests_total",
			Help: "The total number of client requests since APISIX started",
		},
	)

	// FIXME
	EtcdReachable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "etcd_reachable",
			Help: "Config server etcd reachable from APISIX, 0 is unreachable",
		},
	)

	HostInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "node_info",
			Help: "Info of APISIX node",
		}, []string{
			"hostname",
		},
	)

	// FIXME
	EtcdModifyIndexed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "etcd_modify_indexes",
			Help: "Etcd modify index for APISIX keys",
		}, []string{"key"},
	)

	// FIXME
	UpstreamStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "upstream_status",
			Help: "Upstream status from health check",
		}, []string{"name", "ip", "port"},
	)

	// pkg/plugin/request_context/plugin.go
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "http_status",
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
			Name:    metricConfig.MetricPrefix + "http_latency",
			Help:    "HTTP request latency in milliseconds per service in APISIX",
			Buckets: metricConfig.Buckets,
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
			Name: metricConfig.MetricPrefix + "bandwidth",
			Help: "Total bandwidth in bytes consumed per service in APISIX",
		}, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
		},
	)

	BatchProcessEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "batch_process_entries",
			Help: "batch process remaining entries",
		}, []string{
			"name",
			"route_id",
			"server_addr",
		},
	)

	LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "llm_active_connections",
			Help: "Number of active connections to LLM service",
		}, []string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
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
		BatchProcessEntries,
		LLMActiveConnections,
	)
}

func BeginLLMRequest(r *http.Request) func() {
	if LLMActiveConnections == nil {
		return func() {}
	}
	labels := []string{
		requestVarString(r, "$route_name"),
		requestVarString(r, "$route_id"),
		requestVarString(r, "$matched_uri"),
		requestVarString(r, "$host"),
		requestVarString(r, "$service_name"),
		requestVarString(r, "$service_id"),
		requestVarString(r, "$consumer_name"),
		requestVarString(r, "$balancer_ip"),
		requestVarString(r, "$request_type"),
		requestVarString(r, "$request_llm_model"),
		requestVarString(r, "$llm_model"),
	}
	gauge := LLMActiveConnections.WithLabelValues(labels...)
	gauge.Inc()
	return gauge.Dec
}

func requestVarString(r *http.Request, key string) string {
	value := apisixctx.GetRequestVar(r, key)
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func SetBatchProcessEntries(name string, routeID string, serverAddr string, count int) {
	if BatchProcessEntries == nil {
		return
	}
	BatchProcessEntries.WithLabelValues(name, routeID, serverAddr).Set(float64(count))
}

type prometheusMetricConfig struct {
	MetricPrefix string
	Buckets      []float64
}

func newPrometheusMetricConfig(attr map[string]interface{}) prometheusMetricConfig {
	cfg := prometheusMetricConfig{
		MetricPrefix: "apisix_",
		Buckets:      append([]float64(nil), defaultLatencyBuckets...),
	}
	if attr == nil {
		return cfg
	}

	if v, ok := attr["metric_prefix"].(string); ok && v != "" {
		cfg.MetricPrefix = v
	}
	if buckets, ok := parseFloatBuckets(attr["default_buckets"]); ok {
		cfg.Buckets = buckets
	}
	return cfg
}

func parseFloatBuckets(raw interface{}) ([]float64, bool) {
	if raw == nil {
		return nil, false
	}

	switch values := raw.(type) {
	case []float64:
		if len(values) == 0 {
			return nil, false
		}
		return append([]float64(nil), values...), true
	case []interface{}:
		buckets := make([]float64, 0, len(values))
		for _, value := range values {
			bucket, err := cast.ToFloat64E(value)
			if err != nil {
				return nil, false
			}
			buckets = append(buckets, bucket)
		}
		return buckets, len(buckets) > 0
	default:
		return nil, false
	}
}
