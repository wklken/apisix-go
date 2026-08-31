package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricSeriesOverflowMetricsUseFamilyLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	httpOverflow := newHTTPMetricSeriesOverflow("apisix_")
	llmOverflow := newLLMMetricSeriesOverflow("apisix_")
	registry.MustRegister(httpOverflow, llmOverflow)
	httpOverflow.WithLabelValues(httpStatusMetric).Inc()
	llmOverflow.WithLabelValues(llmLatencyMetric).Inc()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	got := make(map[string]string, len(families))
	for _, family := range families {
		got[family.GetName()] = family.Metric[0].GetLabel()[0].GetValue()
	}
	want := map[string]string{
		"apisix_http_metric_series_overflow_total": httpStatusMetric,
		"apisix_llm_metric_series_overflow_total":  llmLatencyMetric,
	}
	for family, label := range want {
		if got[family] != label {
			t.Fatalf("%s metric label = %q, want %q", family, got[family], label)
		}
	}
}

func TestRecordHTTPRequestNormalizesInvalidStatus(t *testing.T) {
	installMetricVectors(t, "test_status_normalized_")
	RecordHTTPRequest(httptest.NewRequest(http.MethodGet, "/", nil), HTTPRequestMetrics{Status: 700})
	status := HttpStatus.WithLabelValues("0", "", "", "", "", "", "", "", "", "", "apisix")
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("normalized status count = %v, want 1", got)
	}
}

func TestRecordHTTPRequestNormalizesStatusExtraLabels(t *testing.T) {
	installMetricVectors(t, "test_status_extra_normalized_")
	prometheusExtraLabels = map[string][]prometheusExtraLabel{
		httpStatusMetric: {
			{Name: "status", Variable: "$status"},
			{Name: "upstream_status", Variable: "$upstream_status"},
		},
	}
	HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_status_extra_normalized_http_status"},
		metricLabelNames(httpStatusMetric, []string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		}),
	)
	RecordHTTPRequest(
		httptest.NewRequest(http.MethodGet, "/", nil),
		HTTPRequestMetrics{Status: 700, UpstreamLatency: 1},
	)
	status := HttpStatus.WithLabelValues(
		"0", "", "", "", "", "", "", "", "", "", "upstream", "0", "0",
	)
	if got := counterValue(t, status); got != 1 {
		t.Fatalf("status with normalized extra labels = %v, want 1", got)
	}
}

func TestInitRetainsInvalidConfigErrorWithoutPublishingMetrics(t *testing.T) {
	const childEnv = "APISIX_GO_INVALID_PROMETHEUS_INIT_CHILD"
	if os.Getenv(childEnv) == "1" {
		attr := map[string]any{"metric_prefix": 42}
		firstErr := Init(attr)
		secondErr := Init(attr)
		if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
			t.Fatalf("Init() errors = %v and %v, want retained identical errors", firstErr, secondErr)
		}
		if HttpStatus != nil || HttpLatency != nil || Bandwidth != nil ||
			httpStatusSeries != nil || httpLatencySeries != nil || bandwidthSeries != nil {
			t.Fatal("invalid Init() published HTTP vectors or series trackers")
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run", "^TestInitRetainsInvalidConfigErrorWithoutPublishingMetrics$")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("invalid Init() child failed: %v\n%s", err, output)
	}
}
