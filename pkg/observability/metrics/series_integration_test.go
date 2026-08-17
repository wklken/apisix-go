package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestRecordHTTPRequestUsesCanonicalOverflowForHTTPAndLLMFamilies(t *testing.T) {
	installTrackedMetricVectors(t, "test_tracked_overflow_", 1, 0)
	request := newLLMMetricsRequest()

	apisixctx.RegisterRequestVar(request, "$route_id", "route-id-a")
	RecordHTTPRequest(request, HTTPRequestMetrics{
		Status:          200,
		Route:           "route-a",
		Service:         "service-a",
		Consumer:        "consumer-a",
		Node:            "node-a",
		RequestLatency:  10,
		UpstreamLatency: 5,
		IngressBytes:    11,
		EgressBytes:     12,
	})
	apisixctx.RegisterRequestVar(request, "$route_id", "route-id-b")
	RecordHTTPRequest(request, HTTPRequestMetrics{
		Status:          201,
		Route:           "route-b",
		Service:         "service-b",
		Consumer:        "consumer-b",
		Node:            "node-b",
		RequestLatency:  20,
		UpstreamLatency: 7,
		IngressBytes:    21,
		EgressBytes:     22,
	})

	for name, tracker := range map[string]*metricSeriesTracker{
		httpStatusMetric:  httpStatusSeries,
		httpLatencyMetric: httpLatencySeries,
		bandwidthMetric:   bandwidthSeries,
		llmLatencyMetric:  llmLatencySeries,
		llmPromptMetric:   llmPromptSeries,
		llmCompleteMetric: llmCompletionSeries,
	} {
		if got := tracker.entryCount(); got != 1 {
			t.Fatalf("%s entryCount() = %d, want 1", name, got)
		}
	}

	if got := counterValue(t, HttpStatus.WithLabelValues(repeatedLabels(11, overflowLabel)...)); got != 1 {
		t.Fatalf("http_status overflow value = %v, want 1", got)
	}
	if got := histogramSampleSum(t, HttpLatency.WithLabelValues(repeatedLabels(8, overflowLabel)...)); got == 0 {
		t.Fatal("http_latency overflow sample sum = 0, want observations")
	}
	if got := counterValue(t, Bandwidth.WithLabelValues(repeatedLabels(8, overflowLabel)...)); got == 0 {
		t.Fatal("bandwidth overflow value = 0, want observations")
	}
	if got := histogramSampleSum(t, LLMLatency.WithLabelValues(repeatedLabels(7, overflowLabel)...)); got == 0 {
		t.Fatal("llm_latency overflow sample sum = 0, want observations")
	}
	if got := counterValue(t, LLMPromptTokens.WithLabelValues(repeatedLabels(7, overflowLabel)...)); got != 7 {
		t.Fatalf("llm_prompt_tokens overflow value = %v, want 7", got)
	}
	if got := counterValue(t, LLMCompletionTokens.WithLabelValues(repeatedLabels(7, overflowLabel)...)); got != 3 {
		t.Fatalf("llm_completion_tokens overflow value = %v, want 3", got)
	}
	for _, name := range []string{httpStatusMetric, httpLatencyMetric, bandwidthMetric} {
		if got := counterValue(t, httpSeriesOverflow.WithLabelValues(name)); got == 0 {
			t.Fatalf("HTTP overflow counter for %s = 0, want observations", name)
		}
	}
	for _, name := range []string{llmLatencyMetric, llmPromptMetric, llmCompleteMetric} {
		if got := counterValue(t, llmSeriesOverflow.WithLabelValues(name)); got == 0 {
			t.Fatalf("LLM overflow counter for %s = 0, want observations", name)
		}
	}
}

func TestMetricFamilyExpirationReleasesHTTPAndLLMCapacity(t *testing.T) {
	installTrackedMetricVectors(t, "test_tracked_re_admit_", 1, time.Minute)
	request := newLLMMetricsRequest()
	now := time.Unix(1000, 0)
	httpStatusSeries.now = func() time.Time { return now }
	llmPromptSeries.now = func() time.Time { return now }

	apisixctx.RegisterRequestVar(request, "$route_id", "route-id-a")
	RecordHTTPRequest(request, HTTPRequestMetrics{
		Status:         200,
		Route:          "route-a",
		Consumer:       "consumer-a",
		Node:           "node-a",
		RequestLatency: 10,
	})
	now = now.Add(time.Minute)
	if got := httpStatusSeries.expireSeries(now, 256); got != 1 {
		t.Fatalf("http_status expireSeries() = %d, want 1", got)
	}
	if got := llmPromptSeries.expireSeries(now, 256); got != 1 {
		t.Fatalf("llm_prompt_tokens expireSeries() = %d, want 1", got)
	}

	apisixctx.RegisterRequestVar(request, "$route_id", "route-id-b")
	RecordHTTPRequest(request, HTTPRequestMetrics{
		Status:         201,
		Route:          "route-b",
		Consumer:       "consumer-b",
		Node:           "node-b",
		RequestLatency: 20,
	})

	status := HttpStatus.WithLabelValues(
		"201", "route-b", "", "", "", "consumer-b", "node-b", "ai_chat", "", "", "apisix",
	)
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("re-admitted http_status value = %v, want 1", got)
	}
	prompt := LLMPromptTokens.WithLabelValues("route-id-b", "", "consumer-b", "node-b", "ai_chat", "", "")
	if got := counterValue(t, prompt); got != 7 {
		t.Fatalf("re-admitted llm_prompt_tokens value = %v, want 7", got)
	}
	if got := counterValue(t, httpSeriesOverflow.WithLabelValues(httpStatusMetric)); got != 0 {
		t.Fatalf("http_status overflow counter = %v, want 0", got)
	}
	if got := counterValue(t, llmSeriesOverflow.WithLabelValues(llmPromptMetric)); got != 0 {
		t.Fatalf("llm_prompt_tokens overflow counter = %v, want 0", got)
	}
}

func TestBeginLLMRequestPinsTrackedGaugeUntilRelease(t *testing.T) {
	installTrackedMetricVectors(t, "test_tracked_active_", 1, time.Minute)
	registry := prometheus.NewRegistry()
	registry.MustRegister(LLMActiveConnections)
	now := time.Unix(2000, 0)
	llmActiveSeries.now = func() time.Time { return now }
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(request, "$route_id", "route-id-a")
	apisixctx.RegisterRequestVar(request, "$request_type", "ai_chat")

	release := BeginLLMRequest(request)
	now = now.Add(time.Minute)
	if got := llmActiveSeries.expireSeries(now, 256); got != 0 {
		t.Fatalf("active expireSeries() = %d, want pinned", got)
	}
	release()
	now = now.Add(time.Minute)
	if got := llmActiveSeries.expireSeries(now, 256); got != 1 {
		t.Fatalf("released expireSeries() = %d, want 1", got)
	}
	if got := gatheredMetricCountFromRegistry(t, registry); got != 0 {
		t.Fatalf("active gauge children after expiration = %d, want 0", got)
	}
}

func TestBeginLLMRequestUsesBoundedOverflowSeries(t *testing.T) {
	installTrackedMetricVectors(t, "test_tracked_active_overflow_", 1, 0)
	first := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(first, "$route_id", "route-id-a")
	apisixctx.RegisterRequestVar(first, "$request_type", "ai_chat")
	firstDone := BeginLLMRequest(first)
	firstDone()

	second := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/", nil))
	apisixctx.RegisterRequestVar(second, "$route_id", "route-id-b")
	apisixctx.RegisterRequestVar(second, "$request_type", "ai_chat")
	secondDone := BeginLLMRequest(second)
	if got := gaugeValue(t, LLMActiveConnections.WithLabelValues(repeatedLabels(11, overflowLabel)...)); got != 1 {
		t.Fatalf("active overflow gauge = %v, want 1", got)
	}
	if got := counterValue(t, llmSeriesOverflow.WithLabelValues(llmActiveMetric)); got != 1 {
		t.Fatalf("active overflow counter = %v, want 1", got)
	}
	secondDone()
}

func installTrackedMetricVectors(t *testing.T, prefix string, limit int, expire time.Duration) {
	t.Helper()
	installMetricVectors(t, prefix)
	old := struct {
		status        *metricSeriesTracker
		latency       *metricSeriesTracker
		bandwidth     *metricSeriesTracker
		llmLatency    *metricSeriesTracker
		llmPrompt     *metricSeriesTracker
		llmCompletion *metricSeriesTracker
		llmActive     *metricSeriesTracker
		httpOverflow  *prometheus.CounterVec
		llmOverflow   *prometheus.CounterVec
	}{
		httpStatusSeries,
		httpLatencySeries,
		bandwidthSeries,
		llmLatencySeries,
		llmPromptSeries,
		llmCompletionSeries,
		llmActiveSeries,
		httpSeriesOverflow,
		llmSeriesOverflow,
	}
	t.Cleanup(func() {
		httpStatusSeries = old.status
		httpLatencySeries = old.latency
		bandwidthSeries = old.bandwidth
		llmLatencySeries = old.llmLatency
		llmPromptSeries = old.llmPrompt
		llmCompletionSeries = old.llmCompletion
		llmActiveSeries = old.llmActive
		httpSeriesOverflow = old.httpOverflow
		llmSeriesOverflow = old.llmOverflow
	})

	LLMActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: prefix + "llm_active_connections"},
		[]string{
			"route", "route_id", "matched_uri", "matched_host", "service", "service_id", "consumer", "node",
			"request_type", "request_llm_model", "llm_model",
		},
	)
	httpSeriesOverflow = newHTTPMetricSeriesOverflow(prefix)
	llmSeriesOverflow = newLLMMetricSeriesOverflow(prefix)
	httpStatusSeries = newMetricSeriesTracker(
		limit, 11, expire, httpSeriesOverflow.WithLabelValues(httpStatusMetric), HttpStatus.DeleteLabelValues,
	)
	httpLatencySeries = newMetricSeriesTracker(
		limit, 8, expire, httpSeriesOverflow.WithLabelValues(httpLatencyMetric), HttpLatency.DeleteLabelValues,
	)
	bandwidthSeries = newMetricSeriesTracker(
		limit, 8, expire, httpSeriesOverflow.WithLabelValues(bandwidthMetric), Bandwidth.DeleteLabelValues,
	)
	llmLatencySeries = newMetricSeriesTracker(
		limit, 7, expire, llmSeriesOverflow.WithLabelValues(llmLatencyMetric), LLMLatency.DeleteLabelValues,
	)
	llmPromptSeries = newMetricSeriesTracker(
		limit, 7, expire, llmSeriesOverflow.WithLabelValues(llmPromptMetric), LLMPromptTokens.DeleteLabelValues,
	)
	llmCompletionSeries = newMetricSeriesTracker(
		limit, 7, expire, llmSeriesOverflow.WithLabelValues(llmCompleteMetric), LLMCompletionTokens.DeleteLabelValues,
	)
	llmActiveSeries = newMetricSeriesTracker(
		limit, 11, expire, llmSeriesOverflow.WithLabelValues(llmActiveMetric), LLMActiveConnections.DeleteLabelValues,
	)
}

func newLLMMetricsRequest() *http.Request {
	request := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/chat", nil))
	apisixctx.RegisterRequestVar(request, "$request_type", "ai_chat")
	apisixctx.RegisterRequestVar(request, "$llm_time_to_first_token", 2.5)
	apisixctx.RegisterRequestVar(request, "$llm_prompt_tokens", 7)
	apisixctx.RegisterRequestVar(request, "$llm_completion_tokens", 3)
	return request
}

func repeatedLabels(count int, value string) []string {
	labels := make([]string, count)
	for index := range labels {
		labels[index] = value
	}
	return labels
}
