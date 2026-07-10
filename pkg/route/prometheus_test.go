package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestBuildRequestContextConfigPassesPrometheusPreferName(t *testing.T) {
	cfg := buildRequestContextConfig(
		resource.Route{
			ID:        "route-1",
			Uri:       "/orders/:id",
			Name:      "route-name",
			ServiceID: "service-1",
		},
		resource.Service{Name: "service-name"},
		map[string]resource.PluginConfig{
			"prometheus": map[string]any{"prefer_name": true},
		},
	)

	if cfg["$route_id"] != "route-1" {
		t.Fatalf("$route_id = %q, want route-1", cfg["$route_id"])
	}
	if cfg["$route_name"] != "route-name" {
		t.Fatalf("$route_name = %q, want route-name", cfg["$route_name"])
	}
	if cfg["$matched_uri"] != "/orders/:id" {
		t.Fatalf("$matched_uri = %q, want /orders/:id", cfg["$matched_uri"])
	}
	if cfg["$service_id"] != "service-1" {
		t.Fatalf("$service_id = %q, want service-1", cfg["$service_id"])
	}
	if cfg["$service_name"] != "service-name" {
		t.Fatalf("$service_name = %q, want service-name", cfg["$service_name"])
	}
	if cfg["$prometheus_prefer_name"] != true {
		t.Fatalf("$prometheus_prefer_name = %v, want true", cfg["$prometheus_prefer_name"])
	}
}

func TestBuildRequestContextConfigDefaultsPrometheusPreferNameFalse(t *testing.T) {
	cfg := buildRequestContextConfig(
		resource.Route{ID: "route-1", Name: "route-name"},
		resource.Service{},
		map[string]resource.PluginConfig{
			"prometheus": map[string]any{},
		},
	)

	if cfg["$prometheus_prefer_name"] != false {
		t.Fatalf("$prometheus_prefer_name = %v, want false", cfg["$prometheus_prefer_name"])
	}
}

func TestInitPluginsPassesRouteIDToLoggerBatchMetrics(t *testing.T) {
	loggerEndpoint := newLoggerEndpoint(t)
	oldBatchProcessEntries := metrics.BatchProcessEntries
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_route_batch_process_entries"},
		[]string{"name", "route_id", "server_addr"},
	)
	metrics.BatchProcessEntries = gauge
	t.Cleanup(func() {
		metrics.BatchProcessEntries = oldBatchProcessEntries
	})

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"http-logger": map[string]any{
				"uri":              loggerEndpoint,
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	httpLogger, ok := plugins[0].(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", plugins[0])
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	if got := routeGaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 1 {
		t.Fatalf("batch_process_entries route label value = %v, want 1", got)
	}
	if got := routeGaugeValue(t, gauge, "http logger", "", "127.0.0.1:9080"); got != 0 {
		t.Fatalf("batch_process_entries empty-route value = %v, want 0", got)
	}
}

func TestInitPluginsPassesServerAddrToLoggerBatchMetrics(t *testing.T) {
	loggerEndpoint := newLoggerEndpoint(t)
	oldBatchProcessEntries := metrics.BatchProcessEntries
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_route_server_batch_process_entries"},
		[]string{"name", "route_id", "server_addr"},
	)
	metrics.BatchProcessEntries = gauge
	t.Cleanup(func() {
		metrics.BatchProcessEntries = oldBatchProcessEntries
	})

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"http-logger": map[string]any{
				"uri":              loggerEndpoint,
				"batch_max_size":   10,
				"buffer_duration":  60,
				"inactive_timeout": 60,
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	httpLogger, ok := plugins[0].(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", plugins[0])
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	if got := routeGaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 1 {
		t.Fatalf("batch_process_entries server label value = %v, want 1", got)
	}
	if got := routeGaugeValue(t, gauge, "http logger", "route-a", ""); got != 0 {
		t.Fatalf("batch_process_entries empty-server value = %v, want 0", got)
	}
}

func TestNewBuilderWithServerAddrNormalizesPortOnlyAddr(t *testing.T) {
	builder := NewBuilderWithServerAddr(nil, ":8080")

	ctx := builder.pluginRouteContext(resource.Route{ID: "route-a"})
	if ctx.serverAddr != "0.0.0.0:8080" {
		t.Fatalf("serverAddr = %q, want 0.0.0.0:8080", ctx.serverAddr)
	}
}

func TestInitGlobalPluginsPassesRouteContextToLoggerBatchMetrics(t *testing.T) {
	loggerEndpoint := newLoggerEndpoint(t)
	oldBatchProcessEntries := metrics.BatchProcessEntries
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_global_route_batch_process_entries"},
		[]string{"name", "route_id", "server_addr"},
	)
	metrics.BatchProcessEntries = gauge
	t.Cleanup(func() {
		metrics.BatchProcessEntries = oldBatchProcessEntries
	})

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
	plugins := builder.initGlobalPlugins(
		[]resource.GlobalRule{
			{
				Plugins: map[string]resource.PluginConfig{
					"http-logger": map[string]any{
						"uri":              loggerEndpoint,
						"batch_max_size":   10,
						"buffer_duration":  60,
						"inactive_timeout": 60,
					},
				},
			},
		},
		builder.pluginRouteContext(resource.Route{ID: "route-a"}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}

	httpLogger, ok := plugins[0].(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", plugins[0])
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.Fire(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("Fire() error = %v", err)
	}

	if got := routeGaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 1 {
		t.Fatalf("global batch_process_entries route/server value = %v, want 1", got)
	}
}

func newLoggerEndpoint(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func routeGaugeValue(t *testing.T, gauge *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()

	metric := &dto.Metric{}
	if err := gauge.WithLabelValues(labels...).Write(metric); err != nil {
		t.Fatalf("read gauge metric: %v", err)
	}
	return metric.GetGauge().GetValue()
}
