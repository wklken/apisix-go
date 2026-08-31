package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cast"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/version"
)

var (
	initOnce sync.Once
	initErr  error
)

var defaultLatencyBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

var (
	Connections           *prometheus.GaugeVec
	Requests              prometheus.Gauge
	EtcdReachable         prometheus.Gauge
	HostInfo              *prometheus.GaugeVec
	EtcdRevision          prometheus.Gauge
	EtcdModifyIndexes     *prometheus.GaugeVec
	UpstreamStatus        *prometheus.GaugeVec
	StreamConnections     *prometheus.CounterVec
	HttpStatus            *prometheus.CounterVec
	HttpLatency           *prometheus.HistogramVec
	Bandwidth             *prometheus.CounterVec
	BatchProcessEntries   *prometheus.GaugeVec
	LLMLatency            *prometheus.HistogramVec
	LLMPromptTokens       *prometheus.CounterVec
	LLMCompletionTokens   *prometheus.CounterVec
	LLMActiveConnections  *prometheus.GaugeVec
	ProxyInFlight         *prometheus.GaugeVec
	ProxyRejected         *prometheus.CounterVec
	ProxyRetry            *prometheus.CounterVec
	ProxyHealth           *prometheus.GaugeVec
	prometheusExtraLabels map[string][]prometheusExtraLabel
	httpStatusSeries      *metricSeriesTracker
	httpLatencySeries     *metricSeriesTracker
	bandwidthSeries       *metricSeriesTracker
	llmLatencySeries      *metricSeriesTracker
	llmPromptSeries       *metricSeriesTracker
	llmCompletionSeries   *metricSeriesTracker
	llmActiveSeries       *metricSeriesTracker
	upstreamStatusSeries  *metricSeriesTracker
	httpSeriesOverflow    *prometheus.CounterVec
	llmSeriesOverflow     *prometheus.CounterVec
	metricExpiration      *expirationRuntime
)

const (
	httpStatusMetric       = "http_status"
	httpLatencyMetric      = "http_latency"
	bandwidthMetric        = "bandwidth"
	llmLatencyMetric       = "llm_latency"
	llmPromptMetric        = "llm_prompt_tokens"
	llmCompleteMetric      = "llm_completion_tokens"
	llmActiveMetric        = "llm_active_connections"
	upstreamStatusMetric   = "upstream_status"
	streamConnectionMetric = "stream_connection_total"
	defaultMaxMetricSeries = 10000
	minMetricSeries        = 100
	maxMetricSeries        = 100000
)

var expirableMetricNames = [...]string{
	httpStatusMetric,
	httpLatencyMetric,
	bandwidthMetric,
	llmLatencyMetric,
	llmPromptMetric,
	llmCompleteMetric,
	llmActiveMetric,
	upstreamStatusMetric,
}

var requestExtraLabelMetricNames = [...]string{
	httpStatusMetric,
	httpLatencyMetric,
	bandwidthMetric,
	llmLatencyMetric,
	llmPromptMetric,
	llmCompleteMetric,
	llmActiveMetric,
}

type prometheusExtraLabel struct {
	Name     string
	Variable string
}

const (
	defaultPrometheusExportURI  = "/apisix/prometheus/metrics"
	defaultPrometheusExportIP   = "127.0.0.1"
	defaultPrometheusExportPort = 9091
)

// PublicEndpointConfig describes where the prometheus plugin should expose
// metrics when the dedicated exporter is disabled.
type PublicEndpointConfig struct {
	Enabled bool
	URI     string
}

// ConfiguredPublicEndpoint validates and returns the configured public-api
// endpoint. The plugin boundary uses this accessor so endpoint routing is
// derived from the same strict configuration contract as the exporter.
func ConfiguredPublicEndpoint(attr map[string]any) (PublicEndpointConfig, error) {
	cfg, err := ConfiguredExportServer(attr)
	if err != nil {
		return PublicEndpointConfig{}, err
	}
	return PublicEndpointConfig{Enabled: cfg.Enabled, URI: cfg.URI}, nil
}

type prometheusEndpointConfig struct {
	Enabled bool
	URI     string
	Address string
}

// ExportServerConfig describes an owned prometheus export HTTP server.
type ExportServerConfig struct {
	Enabled bool
	URI     string
	Address string
}

// ConfiguredExportServer validates the process-level exporter configuration
// and returns the complete owned-server contract for the server lifecycle.
func ConfiguredExportServer(attr map[string]any) (ExportServerConfig, error) {
	cfg, err := configuredPrometheusEndpoint(attr)
	if err != nil {
		return ExportServerConfig{}, err
	}
	return ExportServerConfig(cfg), nil
}

// StartExportServer binds and serves the prometheus export endpoint and
// returns the owned server plus its bound address. The caller must stop the
// returned server during its shutdown path; Stop releases the listener.
func StartExportServer(cfg ExportServerConfig) (*http.Server, net.Addr, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	if err := validatePrometheusURI(cfg.URI, "export_uri"); err != nil {
		return nil, nil, err
	}
	if cfg.Address == "" {
		return nil, nil, errors.New("prometheus export address must not be empty")
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

func configuredPrometheusEndpoint(attr map[string]any) (prometheusEndpointConfig, error) {
	cfg := prometheusEndpointConfig{
		Enabled: true,
		URI:     defaultPrometheusExportURI,
	}
	if attr == nil {
		cfg.Address = net.JoinHostPort(
			defaultPrometheusExportIP,
			strconv.Itoa(defaultPrometheusExportPort),
		)
		return cfg, nil
	}
	if raw, ok := attr["enable_export_server"]; ok {
		enabled, ok := raw.(bool)
		if !ok {
			return cfg, fmt.Errorf(
				"plugin_attr.prometheus.enable_export_server must be a boolean, got %T",
				raw,
			)
		}
		cfg.Enabled = enabled
	}
	if raw, ok := attr["export_uri"]; ok {
		uri, ok := raw.(string)
		if !ok {
			return cfg, fmt.Errorf("plugin_attr.prometheus.export_uri must be a string, got %T", raw)
		}
		if err := validatePrometheusURI(uri, "plugin_attr.prometheus.export_uri"); err != nil {
			return cfg, err
		}
		cfg.URI = uri
	}
	ip := defaultPrometheusExportIP
	port := defaultPrometheusExportPort
	if raw, ok := attr["export_ip"]; ok {
		value, ok := raw.(string)
		if !ok || net.ParseIP(value) == nil {
			return cfg, fmt.Errorf(
				"plugin_attr.prometheus.export_ip must be a literal IP address, got %v",
				raw,
			)
		}
		ip = value
	}
	if raw, ok := attr["export_port"]; ok {
		value, ok := strictEndpointPort(raw)
		if !ok {
			return cfg, fmt.Errorf(
				"plugin_attr.prometheus.export_port must be an integer between 1 and 65535, got %v (%T)",
				raw, raw,
			)
		}
		port = value
	}
	if raw, ok := attr["export_addr"]; ok {
		addr, ok := raw.(map[string]any)
		if !ok {
			return cfg, fmt.Errorf("plugin_attr.prometheus.export_addr must be an object, got %T", raw)
		}
		if rawIP, exists := addr["ip"]; exists {
			value, ok := rawIP.(string)
			if !ok || net.ParseIP(value) == nil {
				return cfg, fmt.Errorf(
					"plugin_attr.prometheus.export_addr.ip must be a literal IP address, got %v",
					rawIP,
				)
			}
			ip = value
		}
		if rawPort, exists := addr["port"]; exists {
			value, ok := strictEndpointPort(rawPort)
			if !ok {
				return cfg, fmt.Errorf(
					"plugin_attr.prometheus.export_addr.port must be an integer between 1 and 65535, got %v (%T)",
					rawPort, rawPort,
				)
			}
			port = value
		}
	}
	cfg.Address = net.JoinHostPort(ip, strconv.Itoa(port))
	return cfg, nil
}

func strictEndpointPort(raw any) (int, bool) {
	value, ok := strictInt64(raw)
	return int(value), ok && value >= 1 && value <= 65535
}

func validatePrometheusURI(uri, field string) error {
	if uri == "" {
		return fmt.Errorf("%s must be a non-empty absolute path", field)
	}
	if strings.IndexFunc(uri, func(r rune) bool {
		switch r {
		case '?', '#', '{', '}', '*', ' ', '\t', '\r', '\n':
			return true
		default:
			return false
		}
	}) >= 0 {
		return fmt.Errorf(
			"%s must be a literal path without query, fragment, whitespace, or wildcard syntax",
			field,
		)
	}
	parsed, err := url.Parse(uri)
	if err != nil ||
		parsed.Path != uri ||
		!strings.HasPrefix(uri, "/") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return fmt.Errorf("%s must be a literal absolute path", field)
	}
	return nil
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

// HTTPRequestMetricContext contains the detached request fields needed to
// resolve HTTP and LLM metric labels. The maps are borrowed only for the
// duration of RecordHTTPRequestContext and are never retained.
type HTTPRequestMetricContext struct {
	Method         string
	Host           string
	Path           string
	APISIXVars     map[string]any
	RequestVars    map[string]any
	ResponseSource apisixctx.ResponseSource
}

func Init(attr map[string]any) error {
	initOnce.Do(func() {
		initErr = initMetrics(attr)
	})
	return initErr
}

func StartExpiration(ctx context.Context) (func(context.Context) error, error) {
	if metricExpiration == nil {
		return nil, nil
	}
	return metricExpiration.Start(ctx)
}

func initMetrics(attr map[string]any) error {
	metricConfig, err := newPrometheusMetricConfig(attr)
	if err != nil {
		return err
	}
	prometheusExtraLabels = metricConfig.ExtraLabels

	Connections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "nginx_http_current_connections",
			Help: "Number of HTTP connections by NGINX-compatible connection state",
		}, []string{"state"},
	)
	for _, state := range []string{"active", "accepted", "handled", "reading", "writing", "waiting"} {
		Connections.WithLabelValues(state).Set(0)
	}

	// pkg/server/server.go
	Requests = prometheus.NewGauge(
		prometheus.GaugeOpts{
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
		}, []string{"hostname", "version"},
	)

	EtcdRevision = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "etcd_revision",
			Help: "Last successfully applied etcd revision",
		},
	)

	EtcdModifyIndexes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "etcd_modify_indexes",
			Help: "Last successfully applied etcd modify index by APISIX resource key",
		}, []string{"key"},
	)

	UpstreamStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "upstream_status",
			Help: "Configured upstream target health, 1 healthy and 0 unhealthy",
		}, []string{"name", "ip", "port"},
	)

	StreamConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + streamConnectionMetric,
			Help: "Completed stream connections by bounded route",
		}, []string{"route"},
	)
	streamRoutes.Lock()
	streamRoutes.ids = nil
	streamRoutes.limit = metricConfig.MaxHTTPSeries
	streamRoutes.overflow = false
	streamRoutes.Unlock()

	// pkg/plugin/prometheus/plugin.go
	httpStatusLabels := metricLabelNames(httpStatusMetric, []string{
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
	})
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "http_status",
			Help: "HTTP status codes per service in APISIX",
		}, httpStatusLabels,
	)

	httpLatencyLabels := metricLabelNames(httpLatencyMetric, []string{
		"type",
		"route",
		"service",
		"consumer",
		"node",
		"request_type",
		"request_llm_model",
		"llm_model",
	})
	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricConfig.MetricPrefix + "http_latency",
			Help:    "HTTP request latency in milliseconds per service in APISIX",
			Buckets: metricConfig.Buckets,
		}, httpLatencyLabels,
	)

	bandwidthLabels := metricLabelNames(bandwidthMetric, []string{
		"type",
		"route",
		"service",
		"consumer",
		"node",
		"request_type",
		"request_llm_model",
		"llm_model",
	})
	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "bandwidth",
			Help: "Total bandwidth in bytes consumed per service in APISIX",
		}, bandwidthLabels,
	)

	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	llmLatencyLabels := metricLabelNames(llmLatencyMetric, llmLabels)
	LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    metricConfig.MetricPrefix + "llm_latency",
			Help:    "LLM request latency in milliseconds",
			Buckets: metricConfig.LLMBuckets,
		},
		llmLatencyLabels,
	)
	llmPromptLabels := metricLabelNames(llmPromptMetric, llmLabels)
	LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "llm_prompt_tokens",
			Help: "LLM service consumed prompt tokens",
		},
		llmPromptLabels,
	)
	llmCompletionLabels := metricLabelNames(llmCompleteMetric, llmLabels)
	LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricConfig.MetricPrefix + "llm_completion_tokens",
			Help: "LLM service consumed completion tokens",
		},
		llmCompletionLabels,
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

	llmActiveLabels := metricLabelNames(llmActiveMetric, []string{
		"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
		"request_type", "request_llm_model", "llm_model",
	})
	LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: metricConfig.MetricPrefix + "llm_active_connections",
			Help: "Number of active connections to LLM service",
		}, llmActiveLabels,
	)

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
	ConfigApplyQuarantined = newConfigApplyQuarantineMetric(nil, metricConfig.MetricPrefix)
	httpSeriesOverflow = newHTTPMetricSeriesOverflow(metricConfig.MetricPrefix)
	llmSeriesOverflow = newLLMMetricSeriesOverflow(metricConfig.MetricPrefix)
	httpStatusSeries = newMetricSeriesTracker(
		metricConfig.MaxHTTPSeries,
		len(httpStatusLabels),
		metricConfig.Expires[httpStatusMetric],
		httpSeriesOverflow.WithLabelValues(httpStatusMetric),
		HttpStatus.DeleteLabelValues,
	)
	httpLatencySeries = newMetricSeriesTracker(
		metricConfig.MaxHTTPSeries,
		len(httpLatencyLabels),
		metricConfig.Expires[httpLatencyMetric],
		httpSeriesOverflow.WithLabelValues(httpLatencyMetric),
		HttpLatency.DeleteLabelValues,
	)
	bandwidthSeries = newMetricSeriesTracker(
		metricConfig.MaxHTTPSeries,
		len(bandwidthLabels),
		metricConfig.Expires[bandwidthMetric],
		httpSeriesOverflow.WithLabelValues(bandwidthMetric),
		Bandwidth.DeleteLabelValues,
	)
	llmLatencySeries = newMetricSeriesTracker(
		metricConfig.MaxLLMSeries,
		len(llmLatencyLabels),
		metricConfig.Expires[llmLatencyMetric],
		llmSeriesOverflow.WithLabelValues(llmLatencyMetric),
		LLMLatency.DeleteLabelValues,
	)
	llmPromptSeries = newMetricSeriesTracker(
		metricConfig.MaxLLMSeries,
		len(llmPromptLabels),
		metricConfig.Expires[llmPromptMetric],
		llmSeriesOverflow.WithLabelValues(llmPromptMetric),
		LLMPromptTokens.DeleteLabelValues,
	)
	llmCompletionSeries = newMetricSeriesTracker(
		metricConfig.MaxLLMSeries,
		len(llmCompletionLabels),
		metricConfig.Expires[llmCompleteMetric],
		llmSeriesOverflow.WithLabelValues(llmCompleteMetric),
		LLMCompletionTokens.DeleteLabelValues,
	)
	llmActiveSeries = newMetricSeriesTracker(
		metricConfig.MaxLLMSeries,
		len(llmActiveLabels),
		metricConfig.Expires[llmActiveMetric],
		llmSeriesOverflow.WithLabelValues(llmActiveMetric),
		LLMActiveConnections.DeleteLabelValues,
	)
	upstreamStatusSeries = newMetricSeriesTracker(
		metricConfig.MaxHTTPSeries,
		3,
		metricConfig.Expires[upstreamStatusMetric],
		httpSeriesOverflow.WithLabelValues(upstreamStatusMetric),
		UpstreamStatus.DeleteLabelValues,
	)
	metricExpiration = newExpirationRuntime(
		httpStatusSeries,
		httpLatencySeries,
		bandwidthSeries,
		llmLatencySeries,
		llmPromptSeries,
		llmCompletionSeries,
		llmActiveSeries,
		upstreamStatusSeries,
	)

	hostName, err := os.Hostname()
	if err != nil || hostName == "" {
		hostName = "unknown"
	}
	HostInfo.WithLabelValues(hostName, version.Version).Set(1)

	for _, collector := range []prometheus.Collector{
		Connections,
		Requests,
		EtcdReachable,
		HostInfo,
		EtcdRevision,
		EtcdModifyIndexes,
		UpstreamStatus,
		StreamConnections,
		HttpStatus,
		HttpLatency,
		Bandwidth,
		httpSeriesOverflow,
		llmSeriesOverflow,
		BatchProcessEntries,
		LoggerBatchPendingEntries,
		LoggerBatchEvents,
		LLMLatency,
		LLMPromptTokens,
		LLMCompletionTokens,
		LLMActiveConnections,
		aiStreamOutcomes,
		functionUpstreamFailures,
		ProxyInFlight,
		ProxyRejected,
		ProxyRetry,
		ProxyHealth,
		requestPanics,
		ConfigApplyFailures,
		ConfigApplyReady,
		ConfigApplyQuarantined,
	} {
		if err := prometheus.Register(collector); err != nil {
			return fmt.Errorf("register prometheus collector: %w", err)
		}
	}
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
	return llmActiveSeries.acquireSeries(
		labels,
		func(actual []string) { LLMActiveConnections.WithLabelValues(actual...).Inc() },
		func(actual []string) { LLMActiveConnections.WithLabelValues(actual...).Dec() },
	)
}

func HTTPRequestMetricsEnabled() bool {
	return HttpStatus != nil &&
		HttpLatency != nil &&
		Bandwidth != nil &&
		LLMLatency != nil &&
		LLMPromptTokens != nil &&
		LLMCompletionTokens != nil
}

func RecordHTTPRequest(r *http.Request, entry HTTPRequestMetrics) {
	if !HTTPRequestMetricsEnabled() || r == nil {
		return
	}
	RecordHTTPRequestContext(metricContextFromRequest(r), entry)
}

// RecordHTTPRequestContext records request metrics from detached request
// metadata. It is synchronous so the context's borrowed maps may be released
// immediately after this function returns.
func RecordHTTPRequestContext(metricContext HTTPRequestMetricContext, entry HTTPRequestMetrics) {
	if !HTTPRequestMetricsEnabled() {
		return
	}
	common := []string{
		entry.Route,
		entry.Service,
		entry.Consumer,
		entry.Node,
		metricContext.requestVarString("$request_type"),
		metricContext.requestVarString("$request_llm_model"),
		metricContext.requestVarString("$llm_model"),
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
		metricContext.responseSource(entry.UpstreamLatency),
	}
	statusLabels = appendExtraLabelValuesContext(httpStatusMetric, metricContext, entry, statusLabels)
	httpStatusSeries.withSeries(statusLabels, func(actual []string) {
		HttpStatus.WithLabelValues(actual...).Inc()
	})

	requestLatencyLabels := appendExtraLabelValuesContext(
		httpLatencyMetric,
		metricContext,
		entry,
		append([]string{"request"}, common...),
	)
	httpLatencySeries.withSeries(requestLatencyLabels, func(actual []string) {
		HttpLatency.WithLabelValues(actual...).Observe(float64(entry.RequestLatency))
	})
	if entry.UpstreamLatency > 0 {
		upstreamLatencyLabels := appendExtraLabelValuesContext(
			httpLatencyMetric,
			metricContext,
			entry,
			append([]string{"upstream"}, common...),
		)
		httpLatencySeries.withSeries(upstreamLatencyLabels, func(actual []string) {
			HttpLatency.WithLabelValues(actual...).Observe(float64(entry.UpstreamLatency))
		})
	}
	apisixLatencyLabels := appendExtraLabelValuesContext(
		httpLatencyMetric,
		metricContext,
		entry,
		append([]string{"apisix"}, common...),
	)
	httpLatencySeries.withSeries(apisixLatencyLabels, func(actual []string) {
		HttpLatency.WithLabelValues(actual...).
			Observe(float64(apisixLatency(entry.RequestLatency, entry.UpstreamLatency)))
	})

	ingressLabels := appendExtraLabelValuesContext(
		bandwidthMetric,
		metricContext,
		entry,
		append([]string{"ingress"}, common...),
	)
	bandwidthSeries.withSeries(ingressLabels, func(actual []string) {
		Bandwidth.WithLabelValues(actual...).Add(float64(entry.IngressBytes))
	})
	egressLabels := appendExtraLabelValuesContext(
		bandwidthMetric,
		metricContext,
		entry,
		append([]string{"egress"}, common...),
	)
	bandwidthSeries.withSeries(egressLabels, func(actual []string) {
		Bandwidth.WithLabelValues(actual...).Add(float64(entry.EgressBytes))
	})

	recordLLMMetricsContext(metricContext, entry)
}

// RecordHTTPRequestTotal records the process-level HTTP request gauge. It is
// intentionally separate from route-owned status/latency recording because
// APISIX's global request counter includes requests without a Prometheus
// route binding.
func RecordHTTPRequestTotal() {
	if Requests != nil {
		Requests.Inc()
	}
}

func normalizedHTTPStatus(status int) string {
	if status < 100 || status > 599 {
		return "0"
	}
	return strconv.Itoa(status)
}

func recordLLMMetricsContext(metricContext HTTPRequestMetricContext, entry HTTPRequestMetrics) {
	requestType := metricContext.requestVarString("$request_type")
	if requestType != "ai_stream" && requestType != "ai_chat" {
		return
	}
	labels := []string{
		metricContext.contextVarString("$route_id"),
		metricContext.contextVarString("$service_id"),
		entry.Consumer,
		entry.Node,
		requestType,
		metricContext.requestVarString("$request_llm_model"),
		metricContext.requestVarString("$llm_model"),
	}
	if firstToken, ok := metricContext.requestVarFloat64("$llm_time_to_first_token"); ok && firstToken != 0 {
		llmLatencyLabels := appendExtraLabelValuesContext(llmLatencyMetric, metricContext, entry, labels)
		llmLatencySeries.withSeries(llmLatencyLabels, func(actual []string) {
			LLMLatency.WithLabelValues(actual...).Observe(firstToken)
		})
	}
	if promptTokens, ok := metricContext.requestVarFloat64("$llm_prompt_tokens"); ok {
		llmPromptLabels := appendExtraLabelValuesContext(llmPromptMetric, metricContext, entry, labels)
		llmPromptSeries.withSeries(llmPromptLabels, func(actual []string) {
			LLMPromptTokens.WithLabelValues(actual...).Add(promptTokens)
		})
	}
	if completionTokens, ok := metricContext.requestVarFloat64("$llm_completion_tokens"); ok {
		llmCompletionLabels := appendExtraLabelValuesContext(llmCompleteMetric, metricContext, entry, labels)
		llmCompletionSeries.withSeries(llmCompletionLabels, func(actual []string) {
			LLMCompletionTokens.WithLabelValues(actual...).Add(completionTokens)
		})
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
	return appendExtraLabelValuesContext(metricName, metricContextFromRequest(r), entry, base)
}

func appendExtraLabelValuesContext(
	metricName string,
	metricContext HTTPRequestMetricContext,
	entry HTTPRequestMetrics,
	base []string,
) []string {
	values := append([]string(nil), base...)
	for _, label := range prometheusExtraLabels[metricName] {
		values = append(values, metricContext.variable(entry, label.Variable))
	}
	return values
}

func metricContextFromRequest(r *http.Request) HTTPRequestMetricContext {
	if r == nil {
		return HTTPRequestMetricContext{}
	}
	metricContext := HTTPRequestMetricContext{
		Method:      r.Method,
		Host:        r.Host,
		APISIXVars:  apisixctx.GetApisixVars(r),
		RequestVars: apisixctx.GetRequestVars(r),
	}
	if r.URL != nil {
		metricContext.Path = r.URL.Path
	}
	return metricContext
}

func (c HTTPRequestMetricContext) variable(entry HTTPRequestMetrics, variable string) string {
	switch variable {
	case "$status":
		return normalizedHTTPStatus(entry.Status)
	case "$upstream_status":
		if entry.UpstreamLatency > 0 {
			return normalizedHTTPStatus(entry.Status)
		}
		return ""
	case "$response_source":
		if c.ResponseSource != "" && c.ResponseSource != apisixctx.ResponseSourceUnknown {
			return string(c.ResponseSource)
		}
	}
	if value := c.requestVarString(variable); value != "" {
		return value
	}
	if value := c.apisixVarString(variable); value != "" {
		return value
	}
	switch variable {
	case "$host":
		return c.Host
	case "$uri":
		return c.Path
	case "$request_method":
		return c.Method
	case "$upstream_addr":
		return entry.Node
	}
	return ""
}

func contextVarString(r *http.Request, key string) string {
	return metricContextFromRequest(r).contextVarString(key)
}

func (c HTTPRequestMetricContext) apisixVarString(key string) string {
	value, ok := c.APISIXVars[key]
	if !ok || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (c HTTPRequestMetricContext) requestVarString(key string) string {
	value, ok := c.RequestVars[key]
	if !ok || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (c HTTPRequestMetricContext) contextVarString(key string) string {
	if value := c.requestVarString(key); value != "" {
		return value
	}
	return c.apisixVarString(key)
}

func (c HTTPRequestMetricContext) requestVarFloat64(key string) (float64, bool) {
	value, ok := c.RequestVars[key]
	if !ok || value == nil {
		return 0, false
	}
	number, err := cast.ToFloat64E(value)
	return number, err == nil
}

func (c HTTPRequestMetricContext) responseSource(upstreamLatency int64) string {
	if c.ResponseSource != "" && c.ResponseSource != apisixctx.ResponseSourceUnknown {
		return string(c.ResponseSource)
	}
	if source := c.requestVarString("$response_source"); source != "" {
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
	return metricContextFromRequest(r).requestVarString(key)
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
	MaxLLMSeries  int
	Expires       map[string]time.Duration
}

func newPrometheusMetricConfig(attr map[string]any) (prometheusMetricConfig, error) {
	cfg := prometheusMetricConfig{
		MetricPrefix:  "apisix_",
		Buckets:       append([]float64(nil), defaultLatencyBuckets...),
		LLMBuckets:    append([]float64(nil), defaultLatencyBuckets...),
		MaxHTTPSeries: defaultMaxMetricSeries,
		MaxLLMSeries:  defaultMaxMetricSeries,
	}
	if attr == nil {
		return cfg, nil
	}
	if raw, ok := attr["max_http_series"]; ok {
		limit, err := parseSeriesLimit(raw, "plugin_attr.prometheus.max_http_series")
		if err != nil {
			return cfg, err
		}
		cfg.MaxHTTPSeries = limit
	}
	if raw, ok := attr["max_llm_series"]; ok {
		limit, err := parseSeriesLimit(raw, "plugin_attr.prometheus.max_llm_series")
		if err != nil {
			return cfg, err
		}
		cfg.MaxLLMSeries = limit
	}

	if raw, ok := attr["metric_prefix"]; ok {
		v, ok := raw.(string)
		if !ok || v == "" {
			return cfg, fmt.Errorf(
				"plugin_attr.prometheus.metric_prefix must be a non-empty string, got %v (%T)",
				raw,
				raw,
			)
		}
		cfg.MetricPrefix = v
	}
	if raw, ok := attr["default_buckets"]; ok {
		buckets, err := parseFloatBuckets(raw, "plugin_attr.prometheus.default_buckets")
		if err != nil {
			return cfg, err
		}
		cfg.Buckets = buckets
	}
	if raw, ok := attr["llm_latency_buckets"]; ok {
		buckets, err := parseFloatBuckets(raw, "plugin_attr.prometheus.llm_latency_buckets")
		if err != nil {
			return cfg, err
		}
		cfg.LLMBuckets = buckets
	}
	if err := validateMetricNames(cfg.MetricPrefix); err != nil {
		return cfg, err
	}
	extraLabels, err := parseExtraLabels(attr["metrics"])
	if err != nil {
		return cfg, err
	}
	cfg.ExtraLabels = extraLabels
	if err := validateMetricConfigEntries(attr["metrics"]); err != nil {
		return cfg, err
	}
	expires, err := parseMetricExpires(attr["metrics"])
	if err != nil {
		return cfg, err
	}
	cfg.Expires = expires
	return cfg, nil
}

func parseSeriesLimit(raw any, fieldName string) (int, error) {
	value, ok := strictInt64(raw)
	if !ok {
		return 0, fmt.Errorf(
			"%s must be an integer between %d and %d, got %T",
			fieldName,
			minMetricSeries,
			maxMetricSeries,
			raw,
		)
	}
	if value < minMetricSeries || value > maxMetricSeries {
		return 0, fmt.Errorf(
			"%s must be between %d and %d, got %d",
			fieldName,
			minMetricSeries,
			maxMetricSeries,
			value,
		)
	}
	return int(value), nil
}

func parseMetricExpires(raw any) (map[string]time.Duration, error) {
	metricConfigs, ok, err := metricConfigMap(raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	var result map[string]time.Duration
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	for _, metricName := range expirableMetricNames {
		metricConfig, exists := metricConfigs[metricName]
		if !exists {
			continue
		}
		configMap, ok := metricConfig.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(
				"plugin_attr.prometheus.metrics.%s must be an object, got %T",
				metricName,
				metricConfig,
			)
		}
		rawExpire, exists := configMap["expire"]
		if !exists {
			continue
		}
		seconds, valid := strictInt64(rawExpire)
		fieldName := "plugin_attr.prometheus.metrics." + metricName + ".expire"
		if !valid || seconds < 0 || seconds > maxSeconds {
			return nil, fmt.Errorf(
				"%s must be a non-negative integer number of seconds, got %v (%T)",
				fieldName,
				rawExpire,
				rawExpire,
			)
		}
		if seconds == 0 {
			continue
		}
		if result == nil {
			result = make(map[string]time.Duration)
		}
		result[metricName] = time.Duration(seconds) * time.Second
	}
	return result, nil
}

func strictInt64(raw any) (int64, bool) {
	switch typed := raw.(type) {
	case json.Number:
		value, err := typed.Int64()
		return value, err == nil
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > uint64(1<<63-1) {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > uint64(1<<63-1) {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}

var jsonNumberPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func strictFloat64(raw any) (float64, bool) {
	switch typed := raw.(type) {
	case json.Number:
		if !jsonNumberPattern.MatchString(typed.String()) {
			return 0, false
		}
		value, err := typed.Float64()
		return value, err == nil
	case float32:
		return float64(typed), true
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func metricConfigMap(raw any) (map[string]any, bool, error) {
	if raw == nil {
		return nil, false, nil
	}
	metricConfigs, ok := raw.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("plugin_attr.prometheus.metrics must be an object, got %T", raw)
	}
	return metricConfigs, true, nil
}

func validateMetricConfigEntries(raw any) error {
	metricConfigs, ok, err := metricConfigMap(raw)
	if err != nil || !ok {
		return err
	}
	known := make(map[string]struct{}, len(configuredMetricSuffixes()))
	for _, name := range configuredMetricSuffixes() {
		known[name] = struct{}{}
	}
	for name, value := range metricConfigs {
		if _, exists := known[name]; !exists {
			continue
		}
		fields, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("plugin_attr.prometheus.metrics.%s must be an object, got %T", name, value)
		}
		for field := range fields {
			if !metricConfigFieldSupported(name, field) {
				return fmt.Errorf("plugin_attr.prometheus.metrics.%s.%s is unsupported", name, field)
			}
		}
	}
	return nil
}

func metricConfigFieldSupported(metricName, field string) bool {
	if field == "expire" {
		return slices.Contains(expirableMetricNames[:], metricName)
	}
	if field == "extra_labels" {
		return slices.Contains(requestExtraLabelMetricNames[:], metricName)
	}
	return false
}

var prometheusNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

func validateMetricNames(prefix string) error {
	for _, suffix := range configuredMetricSuffixes() {
		if !prometheusNamePattern.MatchString(prefix + suffix) {
			return fmt.Errorf(
				"plugin_attr.prometheus.metric_prefix %q produces invalid metric name %q",
				prefix,
				prefix+suffix,
			)
		}
	}
	return nil
}

func validPrometheusLabelName(name string) bool {
	return regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(name)
}

func configuredMetricSuffixes() []string {
	return []string{
		"nginx_http_current_connections", "http_requests_total", "etcd_reachable", "node_info",
		"etcd_revision", "etcd_modify_indexes", "upstream_status", streamConnectionMetric,
		httpStatusMetric, httpLatencyMetric, bandwidthMetric, llmLatencyMetric, llmPromptMetric,
		llmCompleteMetric, "batch_process_entries", llmActiveMetric,
		"ai_stream_outcomes_total", "function_upstream_failures_total", proxyInFlightMetric,
		proxyRejectedMetric, proxyRetryMetric, proxyHealthMetric, requestPanicsMetric,
		configApplyFailuresMetric, configApplyReadyMetric, "http_metric_series_overflow_total",
		"llm_metric_series_overflow_total", "logger_batch_pending_entries", "logger_batch_events_total",
	}
}

func metricBaseLabelNames(metricName string) map[string]struct{} {
	base := map[string][]string{
		httpStatusMetric: {
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
		httpLatencyMetric: {
			"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		},
		bandwidthMetric: {
			"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		},
		llmLatencyMetric: {
			"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		},
		llmPromptMetric: {
			"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		},
		llmCompleteMetric: {
			"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
		},
		llmActiveMetric: {
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		},
		upstreamStatusMetric: {"name", "ip", "port"},
	}
	result := make(map[string]struct{})
	for _, name := range base[metricName] {
		result[name] = struct{}{}
	}
	return result
}

func parseExtraLabels(raw any) (map[string][]prometheusExtraLabel, error) {
	metricConfigs, ok, err := metricConfigMap(raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	if upstreamConfig, exists := metricConfigs[upstreamStatusMetric]; exists {
		configMap, valid := upstreamConfig.(map[string]any)
		if valid {
			if _, configured := configMap["extra_labels"]; configured {
				return nil, fmt.Errorf(
					"plugin_attr.prometheus.metrics.%s.extra_labels is unsupported because upstream status has no request context",
					upstreamStatusMetric,
				)
			}
		}
	}

	result := make(map[string][]prometheusExtraLabel)
	for _, metricName := range requestExtraLabelMetricNames {
		rawConfig, exists := metricConfigs[metricName]
		if !exists {
			continue
		}
		metricConfig, ok := rawConfig.(map[string]any)
		if !ok {
			return nil, fmt.Errorf(
				"plugin_attr.prometheus.metrics.%s must be an object, got %T",
				metricName,
				rawConfig,
			)
		}
		rawLabels, exists := metricConfig["extra_labels"]
		if !exists {
			continue
		}
		labels, ok := rawLabels.([]any)
		if !ok {
			return nil, fmt.Errorf(
				"plugin_attr.prometheus.metrics.%s.extra_labels must be an array, got %T",
				metricName,
				rawLabels,
			)
		}
		baseNames := metricBaseLabelNames(metricName)
		seen := make(map[string]struct{}, len(labels))
		for index, rawLabel := range labels {
			label, ok := rawLabel.(map[string]any)
			if !ok || len(label) != 1 {
				return nil, fmt.Errorf(
					"plugin_attr.prometheus.metrics.%s.extra_labels[%d] must be a one-entry object",
					metricName,
					index,
				)
			}
			for name, rawVariable := range label {
				variable, ok := rawVariable.(string)
				if !validPrometheusLabelName(name) {
					return nil, fmt.Errorf(
						"plugin_attr.prometheus.metrics.%s.extra_labels[%d] has invalid label name %q",
						metricName,
						index,
						name,
					)
				}
				if _, exists := baseNames[name]; exists || name == "le" {
					return nil, fmt.Errorf(
						"plugin_attr.prometheus.metrics.%s.extra_labels label %q collides with a base or reserved label",
						metricName,
						name,
					)
				}
				if _, exists := seen[name]; exists {
					return nil, fmt.Errorf(
						"plugin_attr.prometheus.metrics.%s.extra_labels contains duplicate label %q",
						metricName,
						name,
					)
				}
				if !ok || len(variable) <= 1 || variable[0] != '$' {
					return nil, fmt.Errorf(
						"plugin_attr.prometheus.metrics.%s.extra_labels[%d].%s must be a non-empty variable beginning with $",
						metricName,
						index,
						name,
					)
				}
				seen[name] = struct{}{}
				result[metricName] = append(
					result[metricName],
					prometheusExtraLabel{Name: name, Variable: variable},
				)
			}
		}
	}
	return result, nil
}

func parseFloatBuckets(raw any, fieldName string) ([]float64, error) {
	var values []any
	switch typed := raw.(type) {
	case []any:
		values = typed
	case []float64:
		values = make([]any, len(typed))
		for index, value := range typed {
			values[index] = value
		}
	default:
		return nil, fmt.Errorf("%s must be a non-empty numeric array, got %T", fieldName, raw)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty numeric array", fieldName)
	}
	buckets := make([]float64, len(values))
	for index, rawValue := range values {
		value, ok := strictFloat64(rawValue)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return nil, fmt.Errorf(
				"%s[%d] must be a finite positive number, got %v (%T)",
				fieldName,
				index,
				rawValue,
				rawValue,
			)
		}
		if index > 0 && value <= buckets[index-1] {
			return nil, fmt.Errorf(
				"%s must be strictly increasing, got %v after %v",
				fieldName,
				value,
				buckets[index-1],
			)
		}
		buckets[index] = value
	}
	return buckets, nil
}
