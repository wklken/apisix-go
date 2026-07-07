package metrics

import (
	"reflect"
	"testing"
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
