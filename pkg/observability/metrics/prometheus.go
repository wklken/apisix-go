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

var defaultLatencyBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

// FIXME: how to set etcd reachable?

var (
	Connections           *prometheus.GaugeVec
	Requests              prometheus.Gauge
	EtcdReachable         prometheus.Gauge
	HostInfo              *prometheus.GaugeVec
	EtcdModifyIndexed     *prometheus.GaugeVec
	UpstreamStatus        *prometheus.GaugeVec
	HttpStatus            *prometheus.CounterVec
	HttpLatency           *prometheus.HistogramVec
	Bandwidth             *prometheus.CounterVec
	BatchProcessEntries   *prometheus.GaugeVec
	LLMLatency            *prometheus.HistogramVec
	LLMPromptTokens       *prometheus.CounterVec
	LLMCompletionTokens   *prometheus.CounterVec
	LLMActiveConnections  *prometheus.GaugeVec
	prometheusExtraLabels map[string][]prometheusExtraLabel
)

const (
	httpStatusMetric  = "http_status"
	httpLatencyMetric = "http_latency"
	bandwidthMetric   = "bandwidth"
	llmLatencyMetric  = "llm_latency"
	llmPromptMetric   = "llm_prompt_tokens"
	llmCompleteMetric = "llm_completion_tokens"
	llmActiveMetric   = "llm_active_connections"
)

type prometheusExtraLabel struct {
	Name     string
	Variable string
}

type HTTPRequestMetrics struct {
	Status          int
	Route           string
	MatchedURI      string
	MatchedHost     string
	Service         string
	Consumer        string
	Node            string
	RequestLatency  int64
	UpstreamLatency int64
	IngressBytes    int64
	EgressBytes     int64
}

func Init() {
	var attr map[string]any
	if config.GlobalConfig != nil {
		attr = config.GlobalConfig.PluginAttr["prometheus"]
	}
	metricConfig := newPrometheusMetricConfig(attr)
	prometheusExtraLabels = metricConfig.ExtraLabels

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
		}, metricLabelNames(httpStatusMetric, []string{
			"code",
			"route",
			"matched_uri",
			"matched_host",
			"service",
			"consumer",
			"node",
			"request_type",
			"request_llm_model",
			"llm_model",
			"response_source",
		}),
	)

	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricConfig.MetricPrefix + "http_latency",
			Help:    "HTTP request latency in milliseconds per service in APISIX",
			Buckets: metricConfig.Buckets,
		}, metricLabelNames(httpLatencyMetric, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
			"request_type",
			"request_llm_model",
			"llm_model",
		}),
	)

	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "bandwidth",
			Help: "Total bandwidth in bytes consumed per service in APISIX",
		}, metricLabelNames(bandwidthMetric, []string{
			"type",
			"route",
			"service",
			"consumer",
			"node",
			"request_type",
			"request_llm_model",
			"llm_model",
		}),
	)

	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricConfig.MetricPrefix + "llm_latency",
			Help:    "LLM request latency in milliseconds",
			Buckets: metricConfig.LLMBuckets,
		},
		metricLabelNames(llmLatencyMetric, llmLabels),
	)
	LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "llm_prompt_tokens",
			Help: "LLM service consumed prompt tokens",
		},
		metricLabelNames(llmPromptMetric, llmLabels),
	)
	LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "llm_completion_tokens",
			Help: "LLM service consumed completion tokens",
		},
		metricLabelNames(llmCompleteMetric, llmLabels),
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
		}, metricLabelNames(llmActiveMetric, []string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		}),
	)

	hostName, err := os.Hostname()
	if err != nil || hostName == "" {
		hostName = "unknown"
	}
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
		LLMLatency,
		LLMPromptTokens,
		LLMCompletionTokens,
		LLMActiveConnections,
	)
}

func BeginLLMRequest(r *http.Request) func() {
	if LLMActiveConnections == nil {
		return func() {}
	}
	labels := []string{
		contextVarString(r, "$route_name"),
		contextVarString(r, "$route_id"),
		contextVarString(r, "$matched_uri"),
		contextVarString(r, "$matched_host"),
		contextVarString(r, "$service_name"),
		contextVarString(r, "$service_id"),
		contextVarString(r, "$consumer_name"),
		contextVarString(r, "$balancer_ip"),
		requestVarString(r, "$request_type"),
		requestVarString(r, "$request_llm_model"),
		requestVarString(r, "$llm_model"),
	}
	labels = appendExtraLabelValues(llmActiveMetric, r, HTTPRequestMetrics{}, labels)
	gauge := LLMActiveConnections.WithLabelValues(labels...)
	gauge.Inc()
	return gauge.Dec
}

func RecordHTTPRequest(r *http.Request, entry HTTPRequestMetrics) {
	common := []string{
		entry.Route,
		entry.Service,
		entry.Consumer,
		entry.Node,
		requestVarString(r, "$request_type"),
		requestVarString(r, "$request_llm_model"),
		requestVarString(r, "$llm_model"),
	}
	statusLabels := []string{
		fmt.Sprint(entry.Status),
		entry.Route,
		entry.MatchedURI,
		entry.MatchedHost,
		entry.Service,
		entry.Consumer,
		entry.Node,
		common[4],
		common[5],
		common[6],
		responseSource(r, entry.UpstreamLatency),
	}
	HttpStatus.WithLabelValues(appendExtraLabelValues(httpStatusMetric, r, entry, statusLabels)...).Inc()

	HttpLatency.WithLabelValues(
		appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"request"}, common...))...,
	).Observe(float64(entry.RequestLatency))
	if entry.UpstreamLatency > 0 {
		HttpLatency.WithLabelValues(
			appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"upstream"}, common...))...,
		).Observe(float64(entry.UpstreamLatency))
	}
	HttpLatency.WithLabelValues(
		appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"apisix"}, common...))...,
	).Observe(float64(apisixLatency(entry.RequestLatency, entry.UpstreamLatency)))

	Bandwidth.WithLabelValues(
		appendExtraLabelValues(bandwidthMetric, r, entry, append([]string{"ingress"}, common...))...,
	).Add(float64(entry.IngressBytes))
	Bandwidth.WithLabelValues(
		appendExtraLabelValues(bandwidthMetric, r, entry, append([]string{"egress"}, common...))...,
	).Add(float64(entry.EgressBytes))

	recordLLMMetrics(r, entry)
}

func recordLLMMetrics(r *http.Request, entry HTTPRequestMetrics) {
	requestType := requestVarString(r, "$request_type")
	if requestType != "ai_stream" && requestType != "ai_chat" {
		return
	}
	labels := []string{
		contextVarString(r, "$route_id"),
		contextVarString(r, "$service_id"),
		entry.Consumer,
		entry.Node,
		requestType,
		requestVarString(r, "$request_llm_model"),
		requestVarString(r, "$llm_model"),
	}
	if firstToken, ok := requestVarFloat64(r, "$llm_time_to_first_token"); ok && firstToken != 0 {
		LLMLatency.WithLabelValues(appendExtraLabelValues(llmLatencyMetric, r, entry, labels)...).Observe(firstToken)
	}
	if promptTokens, ok := requestVarFloat64(r, "$llm_prompt_tokens"); ok {
		LLMPromptTokens.WithLabelValues(appendExtraLabelValues(llmPromptMetric, r, entry, labels)...).Add(promptTokens)
	}
	if completionTokens, ok := requestVarFloat64(r, "$llm_completion_tokens"); ok {
		LLMCompletionTokens.WithLabelValues(appendExtraLabelValues(llmCompleteMetric, r, entry, labels)...).Add(
			completionTokens,
		)
	}
}

func metricLabelNames(metricName string, base []string) []string {
	names := append([]string(nil), base...)
	for _, label := range prometheusExtraLabels[metricName] {
		names = append(names, label.Name)
	}
	return names
}

func appendExtraLabelValues(
	metricName string,
	r *http.Request,
	entry HTTPRequestMetrics,
	base []string,
) []string {
	values := append([]string(nil), base...)
	for _, label := range prometheusExtraLabels[metricName] {
		values = append(values, prometheusVariable(r, entry, label.Variable))
	}
	return values
}

func prometheusVariable(r *http.Request, entry HTTPRequestMetrics, variable string) string {
	if value := requestVarString(r, variable); value != "" {
		return value
	}
	if value := apisixVarString(r, variable); value != "" {
		return value
	}
	switch variable {
	case "$host":
		return r.Host
	case "$uri":
		return r.URL.Path
	case "$request_method":
		return r.Method
	case "$status":
		return fmt.Sprint(entry.Status)
	case "$upstream_addr":
		return entry.Node
	case "$upstream_status":
		if entry.UpstreamLatency > 0 {
			return fmt.Sprint(entry.Status)
		}
	}
	return ""
}

func apisixVarString(r *http.Request, key string) string {
	value := apisixctx.GetApisixVar(r, key)
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func contextVarString(r *http.Request, key string) string {
	if value := requestVarString(r, key); value != "" {
		return value
	}
	return apisixVarString(r, key)
}

func requestVarFloat64(r *http.Request, key string) (float64, bool) {
	value := apisixctx.GetRequestVar(r, key)
	if value == nil {
		return 0, false
	}
	number, err := cast.ToFloat64E(value)
	return number, err == nil
}

func responseSource(r *http.Request, upstreamLatency int64) string {
	if source := requestVarString(r, "$response_source"); source != "" {
		return source
	}
	if upstreamLatency > 0 {
		return "upstream"
	}
	return "apisix"
}

func apisixLatency(total int64, upstream int64) int64 {
	if upstream <= 0 {
		return total
	}
	if total <= upstream {
		return 0
	}
	return total - upstream
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
	LLMBuckets   []float64
	ExtraLabels  map[string][]prometheusExtraLabel
}

func newPrometheusMetricConfig(attr map[string]any) prometheusMetricConfig {
	cfg := prometheusMetricConfig{
		MetricPrefix: "apisix_",
		Buckets:      append([]float64(nil), defaultLatencyBuckets...),
		LLMBuckets:   append([]float64(nil), defaultLatencyBuckets...),
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
	if buckets, ok := parseFloatBuckets(attr["llm_latency_buckets"]); ok {
		cfg.LLMBuckets = buckets
	}
	cfg.ExtraLabels = parseExtraLabels(attr["metrics"])
	return cfg
}

func parseExtraLabels(raw any) map[string][]prometheusExtraLabel {
	metricConfigs, ok := raw.(map[string]any)
	if !ok {
		return nil
	}

	result := make(map[string][]prometheusExtraLabel)
	for _, metricName := range []string{
		httpStatusMetric,
		httpLatencyMetric,
		bandwidthMetric,
		llmLatencyMetric,
		llmPromptMetric,
		llmCompleteMetric,
		llmActiveMetric,
	} {
		metricConfig, ok := metricConfigs[metricName].(map[string]any)
		if !ok {
			continue
		}
		labels, ok := metricConfig["extra_labels"].([]any)
		if !ok {
			continue
		}
		for _, rawLabel := range labels {
			label, ok := rawLabel.(map[string]any)
			if !ok || len(label) != 1 {
				continue
			}
			for name, rawVariable := range label {
				variable, ok := rawVariable.(string)
				if name != "" && ok && len(variable) > 1 && variable[0] == '$' {
					result[metricName] = append(result[metricName], prometheusExtraLabel{
						Name: name, Variable: variable,
					})
				}
			}
		}
	}
	return result
}

func parseFloatBuckets(raw any) ([]float64, bool) {
	if raw == nil {
		return nil, false
	}

	switch values := raw.(type) {
	case []float64:
		if len(values) == 0 {
			return nil, false
		}
		return append([]float64(nil), values...), true
	case []any:
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
