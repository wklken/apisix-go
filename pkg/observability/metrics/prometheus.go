package metrics

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cast"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/logger"
)

var (
	initOnce sync.Once
	initErr  error
)

var defaultLatencyBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

var (
	Connections           *prometheus.GaugeVec
	Requests              prometheus.Counter
	EtcdReachable         prometheus.Gauge
	HostInfo              *prometheus.GaugeVec
	EtcdRevision          prometheus.Gauge
	HttpStatus            *prometheus.CounterVec
	HttpLatency           *prometheus.HistogramVec
	Bandwidth             *prometheus.CounterVec
	BatchProcessEntries   *prometheus.GaugeVec
	LLMLatency            *prometheus.HistogramVec
	LLMPromptTokens       *prometheus.CounterVec
	LLMCompletionTokens   *prometheus.CounterVec
	LLMActiveConnections  *prometheus.GaugeVec
	AISafetyOutcomes      *prometheus.CounterVec
	ProxyInFlight         *prometheus.GaugeVec
	ProxyRejected         *prometheus.CounterVec
	ProxyRetry            *prometheus.CounterVec
	ProxyHealth           *prometheus.GaugeVec
	prometheusExtraLabels map[string][]prometheusExtraLabel
	httpStatusBudget      *httpSeriesBudget
	httpLatencyBudget     *httpSeriesBudget
	bandwidthBudget       *httpSeriesBudget
	httpSeriesOverflow    *prometheus.CounterVec
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

// ExportServerConfig describes an owned prometheus export HTTP server.
type ExportServerConfig struct {
	Enabled bool
	URI     string
	Address string
}

// StartExportServer binds and serves the prometheus export endpoint and
// returns the owned server plus its bound address. The caller must stop the
// returned server during its shutdown path; Stop releases the listener.
func StartExportServer(cfg ExportServerConfig) (*http.Server, net.Addr, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	mux := http.NewServeMux()
	mux.Handle(cfg.URI, promhttp.Handler())
	exportServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		return nil, nil, fmt.Errorf("listen prometheus export address %q: %w", cfg.Address, err)
	}
	go func() {
		if err := exportServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Errorf("prometheus export server stopped: %s", err)
		}
	}()
	return exportServer, listener.Addr(), nil
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

func Init() error {
	initOnce.Do(func() {
		initErr = initMetrics()
	})
	return initErr
}

func initMetrics() error {
	var attr map[string]any
	if config.GlobalConfig != nil {
		attr = config.GlobalConfig.PluginAttr["prometheus"]
	}
	metricConfig, err := newPrometheusMetricConfig(attr)
	if err != nil {
		return err
	}
	prometheusExtraLabels = metricConfig.ExtraLabels

	Connections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "http_current_connections",
			Help: "Number of HTTP connections",
		}, []string{"state"},
	)

	// pkg/plugin/request_context/plugin.go
	Requests = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "http_requests_total",
			Help: "The total number of client requests since APISIX started",
		},
	)

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

	EtcdRevision = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "etcd_revision",
			Help: "Last successfully applied etcd revision",
		},
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

	initLoggerBatchMetrics(metricConfig.MetricPrefix)

	LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "llm_active_connections",
			Help: "Number of active connections to LLM service",
		}, metricLabelNames(llmActiveMetric, []string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		}),
	)

	AISafetyOutcomes = newAISafetyOutcomeVector(nil, metricConfig.MetricPrefix)
	aiStreamOutcomes = newAIStreamOutcomeVector(nil, metricConfig.MetricPrefix)
	functionUpstreamFailures = newFunctionUpstreamFailureVector(nil, metricConfig.MetricPrefix)

	ProxyInFlight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + proxyInFlightMetric,
			Help: "Number of concurrently active response bodies in an upstream cluster",
		},
		[]string{"upstream"},
	)
	ProxyRejected = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + proxyRejectedMetric,
			Help: "Total upstream requests rejected because a cluster was overloaded",
		},
		[]string{"upstream"},
	)
	ProxyRetry = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + proxyRetryMetric,
			Help: "Terminal outcome of upstream transport attempts, labelled success/error/stopped",
		},
		[]string{"upstream", "result"},
	)
	ProxyHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + proxyHealthMetric,
			Help: "Current upstream target health, 1 healthy and 0 quarantined",
		},
		[]string{"upstream", "target"},
	)
	requestPanics = newRequestPanicMetrics(nil, metricConfig.MetricPrefix)
	ConfigApplyFailures, ConfigApplyReady = newConfigApplyMetrics(nil, metricConfig.MetricPrefix)
	httpSeriesOverflow = newHTTPMetricSeriesOverflow(metricConfig.MetricPrefix)
	httpStatusBudget = newHTTPSeriesBudgetWithTail(
		metricConfig.MaxHTTPSeries,
		httpSeriesOverflow.WithLabelValues(httpStatusMetric),
		[]int{1, 2, 3, 4, 5, 6, 8, 9},
		11,
	)
	httpLatencyBudget = newHTTPSeriesBudgetWithTail(
		metricConfig.MaxHTTPSeries,
		httpSeriesOverflow.WithLabelValues(httpLatencyMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
	)
	bandwidthBudget = newHTTPSeriesBudgetWithTail(
		metricConfig.MaxHTTPSeries,
		httpSeriesOverflow.WithLabelValues(bandwidthMetric),
		[]int{1, 2, 3, 4, 6, 7},
		8,
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
		EtcdRevision,
		HttpStatus,
		HttpLatency,
		Bandwidth,
		httpSeriesOverflow,
		BatchProcessEntries,
		LoggerBatchPendingEntries,
		LoggerBatchEvents,
		LLMLatency,
		LLMPromptTokens,
		LLMCompletionTokens,
		LLMActiveConnections,
		AISafetyOutcomes,
		aiStreamOutcomes,
		functionUpstreamFailures,
		ProxyInFlight,
		ProxyRejected,
		ProxyRetry,
		ProxyHealth,
		requestPanics,
		ConfigApplyFailures,
		ConfigApplyReady,
	)
	return nil
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

func HTTPRequestMetricsEnabled() bool {
	return Requests != nil &&
		HttpStatus != nil &&
		HttpLatency != nil &&
		Bandwidth != nil &&
		LLMLatency != nil &&
		LLMPromptTokens != nil &&
		LLMCompletionTokens != nil
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
		normalizedHTTPStatus(entry.Status),
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
	statusLabels = appendExtraLabelValues(httpStatusMetric, r, entry, statusLabels)
	statusLabels = admitHTTPMetricLabels(httpStatusMetric, statusLabels)
	HttpStatus.WithLabelValues(statusLabels...).Inc()

	requestLatencyLabels := appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"request"}, common...))
	requestLatencyLabels = admitHTTPMetricLabels(httpLatencyMetric, requestLatencyLabels)
	HttpLatency.WithLabelValues(requestLatencyLabels...).Observe(float64(entry.RequestLatency))
	if entry.UpstreamLatency > 0 {
		upstreamLatencyLabels := appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"upstream"}, common...))
		upstreamLatencyLabels = admitHTTPMetricLabels(httpLatencyMetric, upstreamLatencyLabels)
		HttpLatency.WithLabelValues(upstreamLatencyLabels...).Observe(float64(entry.UpstreamLatency))
	}
	apisixLatencyLabels := appendExtraLabelValues(httpLatencyMetric, r, entry, append([]string{"apisix"}, common...))
	apisixLatencyLabels = admitHTTPMetricLabels(httpLatencyMetric, apisixLatencyLabels)
	HttpLatency.WithLabelValues(apisixLatencyLabels...).Observe(float64(apisixLatency(entry.RequestLatency, entry.UpstreamLatency)))

	ingressLabels := appendExtraLabelValues(bandwidthMetric, r, entry, append([]string{"ingress"}, common...))
	ingressLabels = admitHTTPMetricLabels(bandwidthMetric, ingressLabels)
	Bandwidth.WithLabelValues(ingressLabels...).Add(float64(entry.IngressBytes))
	egressLabels := appendExtraLabelValues(bandwidthMetric, r, entry, append([]string{"egress"}, common...))
	egressLabels = admitHTTPMetricLabels(bandwidthMetric, egressLabels)
	Bandwidth.WithLabelValues(egressLabels...).Add(float64(entry.EgressBytes))

	recordLLMMetrics(r, entry)
}

func admitHTTPMetricLabels(metricName string, labels []string) []string {
	switch metricName {
	case httpStatusMetric:
		return httpStatusBudget.admit(labels)
	case httpLatencyMetric:
		return httpLatencyBudget.admit(labels)
	case bandwidthMetric:
		return bandwidthBudget.admit(labels)
	default:
		return labels
	}
}

func normalizedHTTPStatus(status int) string {
	if status < 100 || status > 599 {
		return "0"
	}
	return strconv.Itoa(status)
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
	switch variable {
	case "$status":
		return normalizedHTTPStatus(entry.Status)
	case "$upstream_status":
		if entry.UpstreamLatency > 0 {
			return normalizedHTTPStatus(entry.Status)
		}
		return ""
	}
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
	case "$upstream_addr":
		return entry.Node
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
	MetricPrefix  string
	Buckets       []float64
	LLMBuckets    []float64
	ExtraLabels   map[string][]prometheusExtraLabel
	MaxHTTPSeries int
}

func newPrometheusMetricConfig(attr map[string]any) (prometheusMetricConfig, error) {
	cfg := prometheusMetricConfig{
		MetricPrefix:  "apisix_",
		Buckets:       append([]float64(nil), defaultLatencyBuckets...),
		LLMBuckets:    append([]float64(nil), defaultLatencyBuckets...),
		MaxHTTPSeries: defaultMaxHTTPSeries,
	}
	if attr == nil {
		return cfg, nil
	}
	if raw, ok := attr["max_http_series"]; ok {
		limit, err := parseMaxHTTPSeries(raw)
		if err != nil {
			return cfg, err
		}
		cfg.MaxHTTPSeries = limit
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
	return cfg, nil
}

func parseMaxHTTPSeries(raw any) (int, error) {
	const fieldName = "plugin_attr.prometheus.max_http_series"
	var value int64
	switch typed := raw.(type) {
	case int:
		value = int64(typed)
	case int8:
		value = int64(typed)
	case int16:
		value = int64(typed)
	case int32:
		value = int64(typed)
	case int64:
		value = typed
	case uint:
		if uint64(typed) > uint64(maxInt()) {
			return 0, fmt.Errorf("%s must be an integer between %d and %d, got %T", fieldName, minHTTPSeries, maxHTTPSeries, raw)
		}
		value = int64(typed)
	case uint8:
		value = int64(typed)
	case uint16:
		value = int64(typed)
	case uint32:
		value = int64(typed)
	case uint64:
		if typed > uint64(maxInt()) {
			return 0, fmt.Errorf("%s must be an integer between %d and %d, got %T", fieldName, minHTTPSeries, maxHTTPSeries, raw)
		}
		value = int64(typed)
	default:
		return 0, fmt.Errorf("%s must be an integer between %d and %d, got %T", fieldName, minHTTPSeries, maxHTTPSeries, raw)
	}
	if value < minHTTPSeries || value > maxHTTPSeries {
		return 0, fmt.Errorf("%s must be between %d and %d, got %d", fieldName, minHTTPSeries, maxHTTPSeries, value)
	}
	return int(value), nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
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
