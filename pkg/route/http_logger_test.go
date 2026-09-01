package route

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestInitPluginsPassesRouteLabelsToHTTPLoggerVariable(t *testing.T) {
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

	bindings := []plugin.Binding{testPluginBinding(
		t,
		"http-logger",
		map[string]any{
			"uri":            logServer.URL,
			"batch_max_size": 1,
			"log_format": map[string]any{
				"labels": "$a6_route_labels",
			},
		},
		resource.Route{
			ID:     "labeled-route",
			Labels: map[string]any{"key": "testvalue"},
		},
	)}
	httpLogger := bindings[0].Plugin.(*http_logger.Plugin)
	t.Cleanup(httpLogger.BatchProcessor.Stop)

	if err := httpLogger.RunLogPhase(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Method: http.MethodGet, URI: "/labels"},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusNoContent},
	}); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}

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
