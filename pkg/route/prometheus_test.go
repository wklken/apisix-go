package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestAPISIXRouteVarsPassRouteMetadata(t *testing.T) {
	routeResource := resource.Route{
		ID:        "route-1",
		Uri:       "/orders/:id",
		Hosts:     []string{"api.example.com"},
		Name:      "route-name",
		ServiceID: "service-1",
	}
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "/orders/42", nil),
		time.Now(),
	)
	t.Cleanup(func() { apisixctx.RecycleVars(request) })
	var got map[string]any
	handler := initializeAPISIXVars(
		http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			got = apisixctx.GetApisixVars(request)
		}),
		"node-1",
		routeResource,
		resource.Service{Name: "service-name"},
	)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	lifecycle.SetFinalRequest(request)

	want := map[string]string{
		"$node_id":      "node-1",
		"$route_id":     "route-1",
		"$route_name":   "route-name",
		"$matched_uri":  "/orders/:id",
		"$matched_host": "api.example.com",
		"$service_id":   "service-1",
		"$service_name": "service-name",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %#v, want %q", name, got[name], value)
		}
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

	binding := testPluginBinding(
		t,
		"http-logger",
		map[string]any{
			"uri":              loggerEndpoint,
			"batch_max_size":   10,
			"buffer_duration":  60,
			"inactive_timeout": 60,
		},
		resource.Route{ID: "route-a"},
	)
	httpLogger, ok := binding.Plugin.(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", binding.Plugin)
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.EnqueueLog(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("EnqueueLog() error = %v", err)
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

	binding := testPluginBinding(
		t,
		"http-logger",
		map[string]any{
			"uri":              loggerEndpoint,
			"batch_max_size":   10,
			"buffer_duration":  60,
			"inactive_timeout": 60,
		},
		resource.Route{ID: "route-a"},
	)
	httpLogger, ok := binding.Plugin.(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", binding.Plugin)
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.EnqueueLog(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("EnqueueLog() error = %v", err)
	}

	if got := routeGaugeValue(t, gauge, "http logger", "route-a", "127.0.0.1:9080"); got != 1 {
		t.Fatalf("batch_process_entries server label value = %v, want 1", got)
	}
	if got := routeGaugeValue(t, gauge, "http logger", "route-a", ""); got != 0 {
		t.Fatalf("batch_process_entries empty-server value = %v, want 0", got)
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

	binding := testPluginBindingForSource(
		t,
		"http-logger",
		map[string]any{
			"uri":              loggerEndpoint,
			"batch_max_size":   10,
			"buffer_duration":  60,
			"inactive_timeout": 60,
		},
		plugin.ScopeGlobal,
		plugin.ResourceProvenance{Kind: plugin.ResourceGlobalRule, ID: "global-logger-metrics"},
		resource.Route{ID: "route-a"},
		resource.Service{},
		"127.0.0.1:9080",
	)
	httpLogger, ok := binding.Plugin.(*http_logger.Plugin)
	if !ok {
		t.Fatalf("plugin type = %T, want *http_logger.Plugin", binding.Plugin)
	}
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.EnqueueLog(map[string]any{"path": "/orders"}); err != nil {
		t.Fatalf("EnqueueLog() error = %v", err)
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
