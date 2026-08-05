package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerPluginChainDoesNotShareRouteBoundPluginsAcrossRoutes(t *testing.T) {
	builder := NewBuilder(nil)
	configs := map[string]resource.PluginConfig{
		"limit-count": map[string]any{
			"count":         1,
			"time_window":   60,
			"key":           "remote_addr",
			"rejected_code": http.StatusTooManyRequests,
		},
	}

	consumer := resource.Consumer{Username: "test-consumer", Plugins: configs}
	firstChain, err := builder.consumerPluginChainForIdentity(
		configs,
		consumer,
		[32]byte{},
		builder.pluginRouteContext(resource.Route{ID: "route-1"}),
	)
	if err != nil {
		t.Fatalf("first consumerPluginChainForIdentity() error = %v", err)
	}
	secondChain, err := builder.consumerPluginChainForIdentity(
		configs,
		consumer,
		[32]byte{},
		builder.pluginRouteContext(resource.Route{ID: "route-2"}),
	)
	if err != nil {
		t.Fatalf("second consumerPluginChainForIdentity() error = %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	for i, chain := range []http.Handler{firstChain.Then(next), secondChain.Then(next)} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		res := httptest.NewRecorder()
		chain.ServeHTTP(res, req)
		if res.Code != http.StatusNoContent {
			t.Fatalf("route %d response code = %d, want %d", i+1, res.Code, http.StatusNoContent)
		}
	}
}
