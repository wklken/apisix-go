package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestInitPluginsPassesRouteLabelsToHTTPLoggerVariable(t *testing.T) {
	ensureRouteStore(t)
	received := make(chan map[string]any, 1)
	logServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode HTTP log: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(logServer.Close)

	builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080", testEffectiveConfig(), testDataEncryptionResolver())
	plugins := builder.initPlugins(
		map[string]resource.PluginConfig{
			"http-logger": map[string]any{
				"uri":            logServer.URL,
				"batch_max_size": 1,
				"log_format": map[string]any{
					"labels": "$a6_route_labels",
				},
			},
		},
		builder.pluginRouteContext(resource.Route{
			ID:     "labeled-route",
			Labels: map[string]any{"key": "testvalue"},
		}),
	)
	if len(plugins) != 1 {
		t.Fatalf("plugins len = %d, want 1", len(plugins))
	}
	httpLogger := plugins[0].(*http_logger.Plugin)
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	httpLogger.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/labels", nil))

	select {
	case body := <-received:
		labels, ok := body["labels"].(map[string]any)
		if !ok || labels["key"] != "testvalue" {
			t.Fatalf("labels = %#v, want route label key=testvalue", body["labels"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP log")
	}
}
