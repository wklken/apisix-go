package metrics

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestPrometheusMetricConfigDefaults(t *testing.T) {
	cfg := newPrometheusMetricConfig(nil)

	if cfg.MetricPrefix != "apisix_" {
		t.Fatalf("MetricPrefix = %q, want apisix_", cfg.MetricPrefix)
	}
	if !reflect.DeepEqual(cfg.Buckets, defaultLatencyBuckets) {
		t.Fatalf("Buckets = %v, want %v", cfg.Buckets, defaultLatencyBuckets)
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
	cfg := newPrometheusMetricConfig(map[string]interface{}{
		"metric_prefix":   "gateway_",
		"default_buckets": []interface{}{10, 50.5, int64(100), "200"},
	})

	if cfg.MetricPrefix != "gateway_" {
		t.Fatalf("MetricPrefix = %q, want gateway_", cfg.MetricPrefix)
	}
	wantBuckets := []float64{10, 50.5, 100, 200}
	if !reflect.DeepEqual(cfg.Buckets, wantBuckets) {
		t.Fatalf("Buckets = %v, want %v", cfg.Buckets, wantBuckets)
	}
}

func TestPrometheusMetricConfigKeepsDefaultsForInvalidBuckets(t *testing.T) {
	cfg := newPrometheusMetricConfig(map[string]interface{}{
		"default_buckets": []interface{}{10, "not-a-number"},
	})

	if !reflect.DeepEqual(cfg.Buckets, defaultLatencyBuckets) {
		t.Fatalf("Buckets = %v, want default %v", cfg.Buckets, defaultLatencyBuckets)
	}
}
