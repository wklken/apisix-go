package metrics

import (
	"context"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"google.golang.org/protobuf/proto"
)

func TestExporterLifecycleBindsServesAndReleasesListener(t *testing.T) {
	exportServer, addr, err := StartExportServer(ExportServerConfig{
		Enabled: true,
		URI:     "/metrics",
		Address: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartExportServer() error = %v", err)
	}
	if exportServer == nil || addr == nil {
		t.Fatal("StartExportServer() returned no owned server or address")
	}

	response, err := http.Get("http://" + addr.String() + "/metrics")
	if err != nil {
		t.Fatalf("scrape export server: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	if err := exportServer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	rebound, err := net.Listen("tcp", addr.String())
	if err != nil {
		t.Fatalf("export server did not release its listener: %v", err)
	}
	_ = rebound.Close()
}

func TestPrometheusMetricConfigDefaults(t *testing.T) {
	cfg, err := newPrometheusMetricConfig(nil)
	if err != nil {
		t.Fatalf("newPrometheusMetricConfig() error = %v", err)
	}

	if cfg.MetricPrefix != "apisix_" {
		t.Fatalf("MetricPrefix = %q, want apisix_", cfg.MetricPrefix)
	}
	if !reflect.DeepEqual(cfg.Buckets, defaultLatencyBuckets) {
		t.Fatalf("Buckets = %v, want %v", cfg.Buckets, defaultLatencyBuckets)
	}
	if !reflect.DeepEqual(cfg.LLMBuckets, defaultLatencyBuckets) {
		t.Fatalf("LLMBuckets = %v, want %v", cfg.LLMBuckets, defaultLatencyBuckets)
	}
	if cfg.MaxLLMSeries != defaultMaxMetricSeries {
		t.Fatalf("MaxLLMSeries = %d, want %d", cfg.MaxLLMSeries, defaultMaxMetricSeries)
	}
	if len(cfg.Expires) != 0 {
		t.Fatalf("Expires = %#v, want disabled defaults", cfg.Expires)
	}
}

func TestPrometheusMetricConfigLLMSeriesLimit(t *testing.T) {
	tests := []struct {
		name    string
		raw     any
		want    int
		wantErr bool
	}{
		{name: "default", want: defaultMaxMetricSeries},
		{name: "minimum", raw: minMetricSeries, want: minMetricSeries},
		{name: "maximum", raw: maxMetricSeries, want: maxMetricSeries},
		{name: "int64", raw: int64(250), want: 250},
		{name: "invalid string", raw: "1000", wantErr: true},
		{name: "invalid bool", raw: true, wantErr: true},
		{name: "invalid fractional", raw: 100.5, wantErr: true},
		{name: "below minimum", raw: minMetricSeries - 1, wantErr: true},
		{name: "above maximum", raw: maxMetricSeries + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attr := map[string]any{}
			if test.raw != nil {
				attr["max_llm_series"] = test.raw
			}
			cfg, err := newPrometheusMetricConfig(attr)
			if test.wantErr {
				if err == nil {
					t.Fatal("newPrometheusMetricConfig() error = nil")
				}
				if !strings.Contains(err.Error(), "plugin_attr.prometheus.max_llm_series") {
					t.Fatalf("error = %v, want full config field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("newPrometheusMetricConfig() error = %v", err)
			}
			if cfg.MaxLLMSeries != test.want {
				t.Fatalf("MaxLLMSeries = %d, want %d", cfg.MaxLLMSeries, test.want)
			}
		})
	}
}

func TestPrometheusMetricConfigParsesMetricExpires(t *testing.T) {
	metricsConfig := make(map[string]any, len(expirableMetricNames))
	for index, name := range expirableMetricNames {
		metricsConfig[name] = map[string]any{"expire": index + 1}
	}

	cfg, err := newPrometheusMetricConfig(map[string]any{"metrics": metricsConfig})
	if err != nil {
		t.Fatalf("newPrometheusMetricConfig() error = %v", err)
	}
	for index, name := range expirableMetricNames {
		want := time.Duration(index+1) * time.Second
		if got := cfg.Expires[name]; got != want {
			t.Fatalf("Expires[%q] = %s, want %s", name, got, want)
		}
	}
}

func TestPrometheusMetricConfigMetricExpireDefaultsToDisabled(t *testing.T) {
	cfg, err := newPrometheusMetricConfig(map[string]any{
		"metrics": map[string]any{
			httpStatusMetric: map[string]any{"expire": 0},
			httpLatencyMetric: map[string]any{
				"extra_labels": []any{map[string]any{"tenant": "$tenant"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("newPrometheusMetricConfig() error = %v", err)
	}
	for _, name := range expirableMetricNames {
		if got := cfg.Expires[name]; got != 0 {
			t.Fatalf("Expires[%q] = %s, want disabled", name, got)
		}
	}
}

func TestPrometheusMetricConfigRejectsInvalidMetricExpire(t *testing.T) {
	tests := []struct {
		name string
		raw  any
	}{
		{name: "negative", raw: -1},
		{name: "fractional", raw: 1.5},
		{name: "string", raw: "60"},
		{name: "boolean", raw: true},
		{name: "overflow", raw: ^uint64(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newPrometheusMetricConfig(map[string]any{
				"metrics": map[string]any{
					httpStatusMetric: map[string]any{"expire": test.raw},
				},
			})
			if err == nil {
				t.Fatal("newPrometheusMetricConfig() error = nil")
			}
			if !strings.Contains(err.Error(), "plugin_attr.prometheus.metrics.http_status.expire") {
				t.Fatalf("error = %v, want full config field", err)
			}
		})
	}
}

func TestBeginLLMRequestUsesStableLabelsForIncrementAndDecrement(t *testing.T) {
	LLMActiveConnections = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "test_llm_active"}, []string{
		"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
		"request_type", "request_llm_model", "llm_model",
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(req, "$request_type", "ai_chat")
	apisixctx.RegisterRequestVar(req, "$request_llm_model", "request-model")
	done := BeginLLMRequest(req)
	gauge := LLMActiveConnections.WithLabelValues(
		"", "", "", "", "", "", "", "", "ai_chat", "request-model", "",
	)
	if got := gaugeValue(t, gauge); got != 1 {
		t.Fatalf("active gauge = %v, want 1", got)
	}
	apisixctx.RegisterRequestVar(req, "$llm_model", "response-model")
	done()
	if got := gaugeValue(t, gauge); got != 0 {
		t.Fatalf("active gauge = %v, want 0", got)
	}
}

func gaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}

func TestPrometheusMetricConfigParsesOfficialPluginAttr(t *testing.T) {
	cfg, err := newPrometheusMetricConfig(map[string]any{
		"metric_prefix":       "gateway_",
		"default_buckets":     []any{10, 50.5, int64(100), 200},
		"llm_latency_buckets": []any{5, 25, 125},
		"metrics": map[string]any{
			"http_status": map[string]any{
				"extra_labels": []any{
					map[string]any{"upstream_addr": "$upstream_addr"},
					map[string]any{"method": "$request_method"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("newPrometheusMetricConfig() error = %v", err)
	}

	if cfg.MetricPrefix != "gateway_" {
		t.Fatalf("MetricPrefix = %q, want gateway_", cfg.MetricPrefix)
	}
	wantBuckets := []float64{10, 50.5, 100, 200}
	if !reflect.DeepEqual(cfg.Buckets, wantBuckets) {
		t.Fatalf("Buckets = %v, want %v", cfg.Buckets, wantBuckets)
	}
	if !reflect.DeepEqual(cfg.LLMBuckets, []float64{5, 25, 125}) {
		t.Fatalf("LLMBuckets = %v, want [5 25 125]", cfg.LLMBuckets)
	}
	wantLabels := []prometheusExtraLabel{
		{Name: "upstream_addr", Variable: "$upstream_addr"},
		{Name: "method", Variable: "$request_method"},
	}
	if !reflect.DeepEqual(cfg.ExtraLabels[httpStatusMetric], wantLabels) {
		t.Fatalf("http_status extra labels = %#v, want %#v", cfg.ExtraLabels[httpStatusMetric], wantLabels)
	}
}

func TestPrometheusMetricConfigRejectsMalformedConfiguration(t *testing.T) {
	tests := []struct {
		name string
		attr map[string]any
	}{
		{"metric prefix type", map[string]any{"metric_prefix": 123}},
		{"metric prefix empty", map[string]any{"metric_prefix": ""}},
		{"metric prefix invalid", map[string]any{"metric_prefix": "gateway-"}},
		{"bucket strings", map[string]any{"default_buckets": []any{"10"}}},
		{"bucket nan", map[string]any{"default_buckets": []any{math.NaN()}}},
		{"bucket infinity", map[string]any{"default_buckets": []any{math.Inf(1)}}},
		{"bucket duplicate", map[string]any{"default_buckets": []any{1, 1}}},
		{"bucket descending", map[string]any{"default_buckets": []any{2, 1}}},
		{"metrics type", map[string]any{"metrics": []any{}}},
		{"known metric type", map[string]any{"metrics": map[string]any{httpStatusMetric: "bad"}}},
		{"official metric type", map[string]any{"metrics": map[string]any{"node_info": "bad"}}},
		{
			"unsupported official expire",
			map[string]any{"metrics": map[string]any{"node_info": map[string]any{"expire": -1}}},
		},
		{
			"unsupported official extra labels",
			map[string]any{"metrics": map[string]any{"node_info": map[string]any{"extra_labels": []any{}}}},
		},
		{
			"unsupported request metric field",
			map[string]any{"metrics": map[string]any{httpStatusMetric: map[string]any{"unknown": true}}},
		},
		{
			"extra labels type",
			map[string]any{"metrics": map[string]any{httpStatusMetric: map[string]any{"extra_labels": "bad"}}},
		},
		{
			"extra label shape",
			map[string]any{"metrics": map[string]any{httpStatusMetric: map[string]any{"extra_labels": []any{"bad"}}}},
		},
		{
			"extra label name",
			map[string]any{
				"metrics": map[string]any{
					httpStatusMetric: map[string]any{
						"extra_labels": []any{map[string]any{"bad-name": "$uri"}},
					},
				},
			},
		},
		{
			"extra label collision",
			map[string]any{
				"metrics": map[string]any{
					httpStatusMetric: map[string]any{
						"extra_labels": []any{map[string]any{"code": "$uri"}},
					},
				},
			},
		},
		{
			"extra label variable",
			map[string]any{
				"metrics": map[string]any{
					httpStatusMetric: map[string]any{
						"extra_labels": []any{map[string]any{"extra": "uri"}},
					},
				},
			},
		},
		{
			"upstream status extra label",
			map[string]any{
				"metrics": map[string]any{
					upstreamStatusMetric: map[string]any{
						"extra_labels": []any{map[string]any{"zone": "$zone"}},
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := newPrometheusMetricConfig(test.attr); err == nil {
				t.Fatal("newPrometheusMetricConfig() error = nil")
			}
		})
	}
}

func TestConfiguredPublicEndpointRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name string
		attr map[string]any
	}{
		{"bool", map[string]any{"enable_export_server": "false"}},
		{"uri", map[string]any{"export_uri": "/metrics?format=text"}},
		{"ip", map[string]any{"export_ip": "localhost"}},
		{"port", map[string]any{"export_port": "9091"}},
		{"nested address", map[string]any{"export_addr": "127.0.0.1:9091"}},
		{"nested port", map[string]any{"export_addr": map[string]any{"port": 0}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ConfiguredPublicEndpoint(test.attr); err == nil {
				t.Fatal("ConfiguredPublicEndpoint() error = nil")
			}
		})
	}
}

func TestConfiguredPublicEndpointUsesCustomURIWhenExporterDisabled(t *testing.T) {
	attr := map[string]any{"enable_export_server": false, "export_uri": "/custom/metrics"}
	endpoint, err := ConfiguredPublicEndpoint(attr)
	if err != nil {
		t.Fatalf("ConfiguredPublicEndpoint() error = %v", err)
	}
	if endpoint.Enabled || endpoint.URI != "/custom/metrics" {
		t.Fatalf("ConfiguredPublicEndpoint() = %#v, want disabled /custom/metrics", endpoint)
	}
}

func TestConfiguredExportServerUsesValidatedAddress(t *testing.T) {
	attr := map[string]any{
		"enable_export_server": true,
		"export_uri":           "/custom/metrics",
		"export_addr": map[string]any{
			"ip":   "0.0.0.0",
			"port": 19091,
		},
	}

	cfg, err := ConfiguredExportServer(attr)
	if err != nil {
		t.Fatalf("ConfiguredExportServer() error = %v", err)
	}
	if !cfg.Enabled || cfg.URI != "/custom/metrics" || cfg.Address != "0.0.0.0:19091" {
		t.Fatalf("ConfiguredExportServer() = %#v, want enabled custom endpoint", cfg)
	}
}

func TestStartExportServerRejectsInvalidURIWithoutPanic(t *testing.T) {
	for _, uri := range []string{"", "metrics", "/metrics?x=1", "/metrics/{name}", "/metrics *"} {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("StartExportServer(%q) panicked: %v", uri, recovered)
				}
			}()
			_, _, err := StartExportServer(ExportServerConfig{
				Enabled: true,
				URI:     uri,
				Address: "127.0.0.1:0",
			})
			if err == nil {
				t.Fatalf("StartExportServer(%q) error = nil", uri)
			}
		}()
	}
}

func TestPrometheusExtraLabelValuesUseRequestAndBoundedHTTPVariables(t *testing.T) {
	oldExtraLabels := prometheusExtraLabels
	prometheusExtraLabels = map[string][]prometheusExtraLabel{
		httpStatusMetric: {
			{Name: "tenant", Variable: "$tenant"},
			{Name: "method", Variable: "$request_method"},
			{Name: "upstream", Variable: "$upstream_addr"},
		},
	}
	t.Cleanup(func() { prometheusExtraLabels = oldExtraLabels })

	req := apisixctx.WithApisixVars(httptest.NewRequest(http.MethodPost, "http://api.example.com/orders", nil), nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$tenant", "acme")
	entry := HTTPRequestMetrics{Status: http.StatusCreated, Node: "10.0.0.8"}
	got := appendExtraLabelValues(httpStatusMetric, req, entry, []string{"base"})
	want := []string{"base", "acme", http.MethodPost, "10.0.0.8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extra label values = %#v, want %#v", got, want)
	}
}

func TestPrometheusStatusVariablesUseDecimalLabels(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	metricContext := metricContextFromRequest(req)
	entry := HTTPRequestMetrics{Status: http.StatusBadGateway, UpstreamLatency: 1}

	if got := metricContext.variable(entry, "$status"); got != "502" {
		t.Fatalf("status label = %q, want 502", got)
	}
	if got := metricContext.variable(entry, "$upstream_status"); got != "502" {
		t.Fatalf("upstream status label = %q, want 502", got)
	}
}

func TestPrometheusMetricConfigKeepsDefaultsForInvalidBuckets(t *testing.T) {
	_, err := newPrometheusMetricConfig(map[string]any{
		"default_buckets": []any{10, "not-a-number"},
	})
	if err == nil {
		t.Fatal("newPrometheusMetricConfig() error = nil for invalid bucket")
	}
}

func installMetricVectors(t *testing.T, prefix string) func() {
	t.Helper()
	old := struct {
		connections          *prometheus.GaugeVec
		requests             prometheus.Gauge
		etcdReachable        prometheus.Gauge
		hostInfo             *prometheus.GaugeVec
		etcdRevision         prometheus.Gauge
		etcdModifyIndexes    *prometheus.GaugeVec
		upstreamStatus       *prometheus.GaugeVec
		streamConnections    *prometheus.CounterVec
		httpStatus           *prometheus.CounterVec
		httpLatency          *prometheus.HistogramVec
		bandwidth            *prometheus.CounterVec
		batchProcessEntries  *prometheus.GaugeVec
		loggerBatchPending   *prometheus.GaugeVec
		loggerBatchEvents    *prometheus.CounterVec
		llmLatency           *prometheus.HistogramVec
		llmPromptTokens      *prometheus.CounterVec
		llmCompletionTokens  *prometheus.CounterVec
		llmActiveConnections *prometheus.GaugeVec
		extraLabels          map[string][]prometheusExtraLabel
		httpStatusSeries     *metricSeriesTracker
		httpLatencySeries    *metricSeriesTracker
		bandwidthSeries      *metricSeriesTracker
		llmLatencySeries     *metricSeriesTracker
		llmPromptSeries      *metricSeriesTracker
		llmCompletionSeries  *metricSeriesTracker
		llmActiveSeries      *metricSeriesTracker
		upstreamStatusSeries *metricSeriesTracker
	}{
		Connections, Requests, EtcdReachable, HostInfo, EtcdRevision,
		EtcdModifyIndexes, UpstreamStatus, StreamConnections,
		HttpStatus, HttpLatency, Bandwidth, BatchProcessEntries, LoggerBatchPendingEntries, LoggerBatchEvents,
		LLMLatency, LLMPromptTokens, LLMCompletionTokens, LLMActiveConnections, prometheusExtraLabels,
		httpStatusSeries, httpLatencySeries, bandwidthSeries,
		llmLatencySeries, llmPromptSeries, llmCompletionSeries, llmActiveSeries, upstreamStatusSeries,
	}
	restore := func() {
		Connections, Requests = old.connections, old.requests
		EtcdReachable, HostInfo, EtcdRevision = old.etcdReachable, old.hostInfo, old.etcdRevision
		EtcdModifyIndexes, UpstreamStatus = old.etcdModifyIndexes, old.upstreamStatus
		StreamConnections = old.streamConnections
		HttpStatus, HttpLatency = old.httpStatus, old.httpLatency
		Bandwidth, BatchProcessEntries = old.bandwidth, old.batchProcessEntries
		LoggerBatchPendingEntries, LoggerBatchEvents = old.loggerBatchPending, old.loggerBatchEvents
		LLMLatency, LLMPromptTokens = old.llmLatency, old.llmPromptTokens
		LLMCompletionTokens, LLMActiveConnections = old.llmCompletionTokens, old.llmActiveConnections
		prometheusExtraLabels = old.extraLabels
		httpStatusSeries, httpLatencySeries = old.httpStatusSeries, old.httpLatencySeries
		bandwidthSeries = old.bandwidthSeries
		llmLatencySeries, llmPromptSeries = old.llmLatencySeries, old.llmPromptSeries
		llmCompletionSeries, llmActiveSeries = old.llmCompletionSeries, old.llmActiveSeries
		upstreamStatusSeries = old.upstreamStatusSeries
	}
	t.Cleanup(restore)

	httpLabels := []string{
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
	}
	commonLabels := []string{
		"type",
		"route",
		"service",
		"consumer",
		"node",
		"request_type",
		"request_llm_model",
		"llm_model",
	}
	llmLabels := []string{
		"route_id",
		"service_id",
		"consumer",
		"node",
		"request_type",
		"request_llm_model",
		"llm_model",
	}
	prometheusExtraLabels = nil
	Requests = prometheus.NewGauge(prometheus.GaugeOpts{Name: prefix + "http_requests_total"})
	EtcdModifyIndexes = nil
	UpstreamStatus = nil
	StreamConnections = nil
	HttpStatus = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "http_status"}, httpLabels)
	HttpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "http_latency"}, commonLabels)
	Bandwidth = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "bandwidth"}, commonLabels)
	LLMLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + "llm_latency"}, llmLabels)
	LLMPromptTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + "llm_prompt_tokens"}, llmLabels)
	LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: prefix + "llm_completion_tokens"},
		llmLabels,
	)
	httpStatusSeries, httpLatencySeries, bandwidthSeries = nil, nil, nil
	llmLatencySeries, llmPromptSeries, llmCompletionSeries, llmActiveSeries = nil, nil, nil, nil
	return restore
}

func TestHTTPRequestMetricsEnabledRequiresAllVectors(t *testing.T) {
	installMetricVectors(t, "test_enable_")

	install := func() {
		Requests = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_enable_requests"})
		HttpStatus = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_enable_status"}, []string{"code"})
		HttpLatency = prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "test_enable_latency"},
			[]string{"type"},
		)
		Bandwidth = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_enable_bw"}, []string{"type"})
		LLMLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_enable_llm_lat"}, []string{"type"})
		LLMPromptTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_enable_prompt"}, []string{"type"})
		LLMCompletionTokens = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_enable_complete"},
			[]string{"type"},
		)
	}
	nilAll := func() {
		Requests, HttpStatus, HttpLatency, Bandwidth = nil, nil, nil, nil
		LLMLatency, LLMPromptTokens, LLMCompletionTokens = nil, nil, nil
	}

	nilAll()
	if HTTPRequestMetricsEnabled() {
		t.Fatal("enabled with no metric vectors installed")
	}
	for _, single := range []func(){
		func() { Requests = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_one_requests"}) },
		func() {
			HttpStatus = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_one_status"}, []string{"code"})
		},
		func() {
			HttpLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_one_latency"}, []string{"type"})
		},
		func() {
			Bandwidth = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_one_bw"}, []string{"type"})
		},
		func() {
			LLMLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "test_one_llm"}, []string{"type"})
		},
		func() {
			LLMPromptTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_one_prompt"}, []string{"type"})
		},
		func() {
			LLMCompletionTokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_one_complete"}, []string{"type"})
		},
	} {
		nilAll()
		single()
		if HTTPRequestMetricsEnabled() {
			t.Fatal("enabled with only one metric vector installed")
		}
	}
	install()
	if !HTTPRequestMetricsEnabled() {
		t.Fatal("disabled with all metric vectors installed")
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func histogramSampleSum(t *testing.T, observer prometheus.Observer) float64 {
	t.Helper()
	histogram, ok := observer.(prometheus.Histogram)
	if !ok {
		t.Fatal("observer does not implement prometheus.Histogram")
	}
	metric := &dto.Metric{}
	if err := histogram.Write(metric); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return metric.GetHistogram().GetSampleSum()
}

func TestRecordHTTPRequestRecordsStatusLatencyBandwidthAndLLM(t *testing.T) {
	installMetricVectors(t, "test_record_")

	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "http://example.com/chat", nil))
	apisixctx.RegisterRequestVar(request, "$request_type", "ai_chat")
	apisixctx.RegisterRequestVar(request, "$llm_time_to_first_token", 12.5)
	apisixctx.RegisterRequestVar(request, "$llm_prompt_tokens", 7)
	apisixctx.RegisterRequestVar(request, "$llm_completion_tokens", 3)
	entry := HTTPRequestMetrics{
		Status:          201,
		RequestLatency:  40,
		UpstreamLatency: 25,
		IngressBytes:    11,
		EgressBytes:     13,
	}

	RecordHTTPRequest(request, entry)

	status := HttpStatus.WithLabelValues("201", "", "", "", "", "", "", "ai_chat", "", "", "upstream")
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("http_status count = %v, want 1", got)
	}
	apisixLatencyHist := HttpLatency.WithLabelValues("apisix", "", "", "", "", "ai_chat", "", "")
	if got := histogramSampleSum(t, apisixLatencyHist); got != 15 {
		t.Fatalf("apisix latency sum = %v, want 15", got)
	}
	ingress := Bandwidth.WithLabelValues("ingress", "", "", "", "", "ai_chat", "", "")
	if got := counterValue(t, ingress); got != 11 {
		t.Fatalf("ingress bandwidth = %v, want 11", got)
	}
	egress := Bandwidth.WithLabelValues("egress", "", "", "", "", "ai_chat", "", "")
	if got := counterValue(t, egress); got != 13 {
		t.Fatalf("egress bandwidth = %v, want 13", got)
	}
	llmLatency := LLMLatency.WithLabelValues("", "", "", "", "ai_chat", "", "")
	if got := histogramSampleSum(t, llmLatency); got != 12.5 {
		t.Fatalf("llm latency sum = %v, want 12.5", got)
	}
	prompt := LLMPromptTokens.WithLabelValues("", "", "", "", "ai_chat", "", "")
	if got := counterValue(t, prompt); got != 7 {
		t.Fatalf("prompt tokens = %v, want 7", got)
	}
	completion := LLMCompletionTokens.WithLabelValues("", "", "", "", "ai_chat", "", "")
	if got := counterValue(t, completion); got != 3 {
		t.Fatalf("completion tokens = %v, want 3", got)
	}
}

func TestRecordHTTPRequestLiveAndDetachedContextsHaveIdenticalMetrics(t *testing.T) {
	extras := map[string][]prometheusExtraLabel{
		httpStatusMetric: {
			{Name: "host", Variable: "$host"},
			{Name: "uri", Variable: "$uri"},
			{Name: "method", Variable: "$request_method"},
			{Name: "request_tenant", Variable: "$request_tenant"},
			{Name: "apisix_tenant", Variable: "$apisix_tenant"},
		},
		httpLatencyMetric: {
			{Name: "host", Variable: "$host"},
			{Name: "uri", Variable: "$uri"},
			{Name: "method", Variable: "$request_method"},
			{Name: "request_tenant", Variable: "$request_tenant"},
			{Name: "apisix_tenant", Variable: "$apisix_tenant"},
		},
		bandwidthMetric: {
			{Name: "host", Variable: "$host"},
			{Name: "uri", Variable: "$uri"},
			{Name: "method", Variable: "$request_method"},
			{Name: "request_tenant", Variable: "$request_tenant"},
			{Name: "apisix_tenant", Variable: "$apisix_tenant"},
		},
	}

	request := apisixctx.WithApisixVars(
		httptest.NewRequest(http.MethodPost, "https://api.example.test/orders/42?q=one", nil),
		nil,
	)
	request = apisixctx.WithRequestVars(request)
	apisixctx.RegisterApisixVar(request, "$route_id", "route-42")
	apisixctx.RegisterApisixVar(request, "$service_id", "service-42")
	apisixctx.RegisterApisixVar(request, "$apisix_tenant", "apisix-acme")
	apisixctx.RegisterRequestVar(request, "$request_type", "ai_chat")
	apisixctx.RegisterRequestVar(request, "$request_llm_model", "request-model")
	apisixctx.RegisterRequestVar(request, "$llm_model", "response-model")
	apisixctx.RegisterRequestVar(request, "$request_tenant", "request-acme")
	apisixctx.RegisterRequestVar(request, "$response_source", "ai-proxy")
	apisixctx.RegisterRequestVar(request, "$llm_time_to_first_token", 12.5)
	apisixctx.RegisterRequestVar(request, "$llm_prompt_tokens", 7)
	apisixctx.RegisterRequestVar(request, "$llm_completion_tokens", 3)
	entry := HTTPRequestMetrics{
		Status:          http.StatusCreated,
		Route:           "route-42",
		MatchedURI:      "/orders/:id",
		MatchedHost:     "api.example.test",
		Service:         "service-42",
		Consumer:        "alice",
		Node:            "10.0.0.8",
		RequestLatency:  40,
		UpstreamLatency: 25,
		IngressBytes:    11,
		EgressBytes:     13,
	}
	detachedContext := HTTPRequestMetricContext{
		Method:      request.Method,
		Host:        request.Host,
		Path:        request.URL.Path,
		APISIXVars:  apisixctx.GetApisixVars(request),
		RequestVars: apisixctx.GetRequestVars(request),
	}

	record := func(t *testing.T, prefix string, useDetached bool) map[string]map[string]*dto.Metric {
		t.Helper()
		restore := installMetricVectorsWithExtras(t, prefix, extras)
		if useDetached {
			RecordHTTPRequestContext(detachedContext, entry)
		} else {
			RecordHTTPRequest(request, entry)
		}
		got := map[string]map[string]*dto.Metric{
			httpStatusMetric:  collectMetricSnapshots(t, HttpStatus),
			httpLatencyMetric: collectMetricSnapshots(t, HttpLatency),
			bandwidthMetric:   collectMetricSnapshots(t, Bandwidth),
			llmLatencyMetric:  collectMetricSnapshots(t, LLMLatency),
			llmPromptMetric:   collectMetricSnapshots(t, LLMPromptTokens),
			llmCompleteMetric: collectMetricSnapshots(t, LLMCompletionTokens),
		}
		restore()
		return got
	}

	live := record(t, "test_live_detached_", false)
	detachedMetrics := record(t, "test_detached_", true)
	if len(live) != len(detachedMetrics) {
		t.Fatalf("live and detached metric families = %d/%d, want equal", len(live), len(detachedMetrics))
	}
	for family, liveSamples := range live {
		detachedSamples := detachedMetrics[family]
		if len(liveSamples) != len(detachedSamples) {
			t.Fatalf(
				"%s live and detached sample counts = %d/%d, want equal",
				family,
				len(liveSamples),
				len(detachedSamples),
			)
		}
		for labels, liveMetric := range liveSamples {
			detachedMetric := detachedSamples[labels]
			if detachedMetric == nil || !proto.Equal(liveMetric, detachedMetric) {
				t.Fatalf("%s labels %q differ between live and detached metrics", family, labels)
			}
		}
	}
}

func installMetricVectorsWithExtras(
	t *testing.T,
	prefix string,
	extraLabels map[string][]prometheusExtraLabel,
) func() {
	t.Helper()
	restore := installMetricVectors(t, prefix)
	prometheusExtraLabels = extraLabels
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: prefix + "http_status"},
		metricLabelNames(httpStatusMetric, []string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		}),
	)
	commonLabels := []string{
		"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: prefix + "http_latency"},
		metricLabelNames(httpLatencyMetric, commonLabels),
	)
	Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: prefix + "bandwidth"},
		metricLabelNames(bandwidthMetric, commonLabels),
	)
	return restore
}

func collectMetricSnapshots(t *testing.T, collector prometheus.Collector) map[string]*dto.Metric {
	t.Helper()
	if collector == nil {
		return nil
	}
	channel := make(chan prometheus.Metric)
	go func() {
		collector.Collect(channel)
		close(channel)
	}()
	snapshots := make(map[string]*dto.Metric)
	for metric := range channel {
		decoded := &dto.Metric{}
		if err := metric.Write(decoded); err != nil {
			t.Fatalf("write metric: %v", err)
		}
		if decoded.Counter != nil {
			decoded.Counter.CreatedTimestamp = nil
		}
		if decoded.Histogram != nil {
			decoded.Histogram.CreatedTimestamp = nil
		}
		labels := make([]string, 0, len(decoded.Label))
		for _, label := range decoded.Label {
			labels = append(labels, label.GetName()+"="+label.GetValue())
		}
		key := strings.Join(labels, "\x00")
		snapshots[key] = decoded
	}
	return snapshots
}

func TestResponseSourcePrecedence(t *testing.T) {
	explicit := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	apisixctx.RegisterRequestVar(explicit, "$response_source", "ai-proxy")
	if got := metricContextFromRequest(explicit).responseSource(25); got != "ai-proxy" {
		t.Fatalf("explicit response_source = %q, want ai-proxy", got)
	}
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	metricContext := metricContextFromRequest(plain)
	if got := metricContext.responseSource(0); got != "apisix" {
		t.Fatalf("no-upstream response_source = %q, want apisix", got)
	}
	if got := metricContext.responseSource(5); got != "upstream" {
		t.Fatalf("upstream response_source = %q, want upstream", got)
	}
}

func TestDetachedResponseSourcePrecedence(t *testing.T) {
	metricContext := HTTPRequestMetricContext{
		RequestVars:    map[string]any{"$response_source": "request"},
		ResponseSource: apisixctx.ResponseSourceCacheHit,
	}
	if got := metricContext.responseSource(25); got != string(apisixctx.ResponseSourceCacheHit) {
		t.Fatalf("detached response_source = %q, want cache_hit", got)
	}
	extraLabel := metricContext.variable(HTTPRequestMetrics{}, "$response_source")
	if extraLabel != string(apisixctx.ResponseSourceCacheHit) {
		t.Fatalf("detached response_source extra label = %q, want cache_hit", extraLabel)
	}
	metricContext.ResponseSource = apisixctx.ResponseSourceUnknown
	if got := metricContext.responseSource(25); got != "request" {
		t.Fatalf("unknown detached response_source = %q, want request", got)
	}
	extraLabel = metricContext.variable(HTTPRequestMetrics{}, "$response_source")
	if extraLabel != "request" {
		t.Fatalf("unknown detached response_source extra label = %q, want request", extraLabel)
	}
}

func TestRecordHTTPRequestIgnoresNilRequest(t *testing.T) {
	installMetricVectors(t, "test_nil_request_")
	RecordHTTPRequest(nil, HTTPRequestMetrics{Status: http.StatusNoContent})
	status := HttpStatus.WithLabelValues("204", "", "", "", "", "", "", "", "", "", "apisix")
	if got := counterValue(t, status); got != 0 {
		t.Fatalf("nil request status count = %v, want 0", got)
	}
}

func TestApisixLatencyBoundaries(t *testing.T) {
	tests := []struct {
		total    int64
		upstream int64
		want     int64
	}{
		{total: 40, upstream: 0, want: 40},
		{total: 10, upstream: 20, want: 0},
		{total: 20, upstream: 20, want: 0},
		{total: 40, upstream: 25, want: 15},
	}
	for _, test := range tests {
		if got := apisixLatency(test.total, test.upstream); got != test.want {
			t.Fatalf("apisixLatency(%d, %d) = %d, want %d", test.total, test.upstream, got, test.want)
		}
	}
}

func TestRequestVarFloat64Conversions(t *testing.T) {
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodGet, "/", nil))
	apisixctx.RegisterRequestVar(request, "$int", 7)
	apisixctx.RegisterRequestVar(request, "$numeric", "3.5")
	apisixctx.RegisterRequestVar(request, "$invalid", "not-a-number")
	metricContext := metricContextFromRequest(request)

	if got, ok := metricContext.requestVarFloat64("$int"); !ok || got != 7 {
		t.Fatalf("int var = %v/%t, want 7/true", got, ok)
	}
	if got, ok := metricContext.requestVarFloat64("$numeric"); !ok || got != 3.5 {
		t.Fatalf("numeric string var = %v/%t, want 3.5/true", got, ok)
	}
	if got, ok := metricContext.requestVarFloat64("$invalid"); ok || got != 0 {
		t.Fatalf("invalid var = %v/%t, want 0/false", got, ok)
	}
	if got, ok := metricContext.requestVarFloat64("$absent"); ok || got != 0 {
		t.Fatalf("absent var = %v/%t, want 0/false", got, ok)
	}
}

func TestMetricLabelNamesDoesNotMutateBase(t *testing.T) {
	installMetricVectors(t, "test_labels_")
	prometheusExtraLabels = map[string][]prometheusExtraLabel{
		httpStatusMetric: {{Name: "tenant", Variable: "$tenant"}},
	}

	base := []string{"code", "route"}
	names := metricLabelNames(httpStatusMetric, base)
	if len(names) != 3 {
		t.Fatalf("names = %v, want base plus one extra label", names)
	}
	if len(base) != 2 {
		t.Fatalf("base mutated to %v, want two entries", base)
	}
}

func TestSetBatchProcessEntriesNilAndSet(t *testing.T) {
	restore := installMetricVectors(t, "test_batch_")

	BatchProcessEntries = nil
	SetBatchProcessEntries("logger", "route-1", "127.0.0.1", 5)

	BatchProcessEntries = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_batch_entries"},
		[]string{"name", "route_id", "server_addr"},
	)
	SetBatchProcessEntries("logger", "route-1", "127.0.0.1", 5)
	gauge := BatchProcessEntries.WithLabelValues("logger", "route-1", "127.0.0.1")
	if got := gaugeValue(t, gauge); got != 5 {
		t.Fatalf("batch entries = %v, want 5", got)
	}
	_ = restore
}

func TestInitInstallsVectorsAndEnablement(t *testing.T) {
	attr := map[string]any{
		"metric_prefix": "unit_",
		"metrics": map[string]any{
			httpStatusMetric: map[string]any{"expire": 1},
		},
	}

	if err := Init(attr); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := Init(attr); err != nil {
		t.Fatalf("second Init() error = %v", err)
	}

	if !HTTPRequestMetricsEnabled() {
		t.Fatal("HTTPRequestMetricsEnabled() = false after Init()")
	}
	if Requests == nil || HttpStatus == nil || HttpLatency == nil || Bandwidth == nil {
		t.Fatal("Init() did not install all HTTP metric vectors")
	}
	if LLMLatency == nil || LLMPromptTokens == nil || LLMCompletionTokens == nil {
		t.Fatal("Init() did not install all LLM metric vectors")
	}
	if httpStatusSeries == nil || httpLatencySeries == nil || bandwidthSeries == nil ||
		llmLatencySeries == nil || llmPromptSeries == nil || llmCompletionSeries == nil || llmActiveSeries == nil {
		t.Fatal("Init() did not install all metric series trackers")
	}
	if requestPanics == nil {
		t.Fatal("Init() did not install request panic metric")
	}
	RecordHTTPRequestTotal()
	RecordEtcdAppliedRevision(42)
	setUpstreamStatus("orders", "127.0.0.1:8080", true)
	SetStreamRoutes([]string{"stream-route"})
	RecordStreamConnection("stream-route")
	t.Cleanup(func() { SetStreamRoutes(nil) })

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather initialized metrics: %v", err)
	}
	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	for _, contract := range []struct {
		name   string
		typeID dto.MetricType
		labels []string
	}{
		{name: "unit_nginx_http_current_connections", typeID: dto.MetricType_GAUGE, labels: []string{"state"}},
		{name: "unit_http_requests_total", typeID: dto.MetricType_GAUGE},
		{name: "unit_node_info", typeID: dto.MetricType_GAUGE, labels: []string{"hostname", "version"}},
		{name: "unit_etcd_modify_indexes", typeID: dto.MetricType_GAUGE, labels: []string{"key"}},
		{name: "unit_upstream_status", typeID: dto.MetricType_GAUGE, labels: []string{"name", "ip", "port"}},
		{name: "unit_stream_connection_total", typeID: dto.MetricType_COUNTER, labels: []string{"route"}},
	} {
		family := byName[contract.name]
		if family == nil {
			t.Errorf("official metric family %q was not gathered", contract.name)
			continue
		}
		if got := family.GetType(); got != contract.typeID {
			t.Errorf("metric family %q type = %s, want %s", contract.name, got, contract.typeID)
		}
		if len(family.Metric) == 0 {
			t.Errorf("metric family %q has no children", contract.name)
			continue
		}
		gotLabels := make(map[string]struct{}, len(family.Metric[0].Label))
		for _, pair := range family.Metric[0].Label {
			gotLabels[pair.GetName()] = struct{}{}
		}
		if len(gotLabels) != len(contract.labels) {
			t.Errorf("metric family %q labels = %v, want %v", contract.name, gotLabels, contract.labels)
			continue
		}
		for _, label := range contract.labels {
			if _, ok := gotLabels[label]; !ok {
				t.Errorf("metric family %q is missing label %q", contract.name, label)
			}
		}
	}
	stop, err := StartExpiration(context.Background())
	if err != nil {
		t.Fatalf("StartExpiration() error = %v", err)
	}
	if stop == nil {
		t.Fatal("StartExpiration() stop = nil with enabled expiration")
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop expiration error = %v", err)
	}
}

func TestPrometheusVariableBoundedFallbacks(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://api.example.test/orders", nil)
	request.Host = "api.example.test"
	metricContext := metricContextFromRequest(request)
	entry := HTTPRequestMetrics{Status: 201, Node: "10.0.0.8", UpstreamLatency: 25}

	tests := map[string]string{
		"$status":           "201",
		"$upstream_addr":    "10.0.0.8",
		"$upstream_status":  "201",
		"$host":             "api.example.test",
		"$uri":              "/orders",
		"$request_method":   "POST",
		"$unknown_variable": "",
	}
	for variable, want := range tests {
		if got := metricContext.variable(entry, variable); got != want {
			t.Fatalf("prometheusVariable(%q) = %q, want %q", variable, got, want)
		}
	}

	noUpstream := HTTPRequestMetrics{Status: 201, Node: "10.0.0.8"}
	if got := metricContext.variable(noUpstream, "$upstream_status"); got != "" {
		t.Fatalf("$upstream_status without upstream = %q, want empty", got)
	}
}
