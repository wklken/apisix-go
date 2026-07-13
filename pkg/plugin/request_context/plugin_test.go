package request_context

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
)

func TestMetricLabelsDefaultUseIDs(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:     "route-1",
			RouteName:   "route-name",
			ServiceID:   "service-1",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-1" {
		t.Fatalf("route label = %q, want route-1", labels.route)
	}
	if labels.service != "service-1" {
		t.Fatalf("service label = %q, want service-1", labels.service)
	}
}

func TestMetricLabelsPreferNameUsesNames(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:              "route-1",
			RouteName:            "route-name",
			ServiceID:            "service-1",
			ServiceName:          "service-name",
			PrometheusPreferName: true,
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}

func TestMetricLabelsFallbackToNameWhenIDMissing(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteName:   "route-name",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}

func TestHandlerRecordsOfficialPrometheusRequestMetrics(t *testing.T) {
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldLLMPromptTokens := metrics.LLMPromptTokens
	oldLLMCompletionTokens := metrics.LLMCompletionTokens
	metrics.Requests = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_prometheus_requests"})
	metrics.HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_http_status"},
		[]string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
	)
	metrics.HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_prometheus_http_latency"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	metrics.Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_bandwidth"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	metrics.LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_prometheus_llm_latency"}, llmLabels,
	)
	metrics.LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_llm_prompt_tokens"}, llmLabels,
	)
	metrics.LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_llm_completion_tokens"}, llmLabels,
	)
	t.Cleanup(func() {
		metrics.Requests = oldRequests
		metrics.HttpStatus = oldStatus
		metrics.HttpLatency = oldLatency
		metrics.Bandwidth = oldBandwidth
		metrics.LLMLatency = oldLLMLatency
		metrics.LLMPromptTokens = oldLLMPromptTokens
		metrics.LLMCompletionTokens = oldLLMCompletionTokens
	})

	p := &Plugin{config: Config{
		RouteID:     "route-1",
		MatchedURI:  "/orders/:id",
		MatchedHost: "api.example.com",
		ServiceID:   "service-1",
	}}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$consumer_name", "alice")
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(1))
		apisixctx.RegisterRequestVar(r, "$request_type", "ai_chat")
		apisixctx.RegisterRequestVar(r, "$request_llm_model", "gpt-request")
		apisixctx.RegisterRequestVar(r, "$llm_model", "gpt-upstream")
		apisixctx.RegisterRequestVar(r, "$llm_time_to_first_token", int64(12))
		apisixctx.RegisterRequestVar(r, "$llm_prompt_tokens", int64(23))
		apisixctx.RegisterRequestVar(r, "$llm_completion_tokens", int64(8))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.com/orders/42", nil)
	req.ContentLength = 7
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"201", "route-1", "/orders/:id", "api.example.com", "service-1", "alice", "10.0.0.8",
		"ai_chat", "gpt-request", "gpt-upstream", "upstream",
	)); got != 1 {
		t.Fatalf("http status count = %v, want 1", got)
	}
	for _, metricType := range []string{"request", "upstream", "apisix"} {
		if got := histogramCount(t, metrics.HttpLatency.WithLabelValues(
			metricType, "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
		)); got != 1 {
			t.Fatalf("%s latency count = %d, want 1", metricType, got)
		}
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"ingress", "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	)); got != 7 {
		t.Fatalf("ingress bandwidth = %v, want 7", got)
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"egress", "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	)); got != 5 {
		t.Fatalf("egress bandwidth = %v, want 5", got)
	}
	llmLabelValues := []string{
		"route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	}
	if got := histogramCount(t, metrics.LLMLatency.WithLabelValues(llmLabelValues...)); got != 1 {
		t.Fatalf("LLM latency count = %d, want 1", got)
	}
	if got := counterValue(t, metrics.LLMPromptTokens.WithLabelValues(llmLabelValues...)); got != 23 {
		t.Fatalf("LLM prompt tokens = %v, want 23", got)
	}
	if got := counterValue(t, metrics.LLMCompletionTokens.WithLabelValues(llmLabelValues...)); got != 8 {
		t.Fatalf("LLM completion tokens = %v, want 8", got)
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func histogramCount(t *testing.T, observer prometheus.Observer) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	writer, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T does not implement prometheus.Metric", observer)
	}
	if err := writer.Write(metric); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return metric.GetHistogram().GetSampleCount()
}
