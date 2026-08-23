package route

import (
	"net/http"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerPluginChainDoesNotShareRouteBoundPluginsAcrossRoutes(t *testing.T) {
	builder := NewBuilder(nil, testEffectiveConfig(), testDataEncryptionResolver())
	configs := map[string]resource.PluginConfig{
		"limit-count": map[string]any{
			"count":         1,
			"time_window":   60,
			"key":           "remote_addr",
			"rejected_code": http.StatusTooManyRequests,
		},
	}

	consumer := resource.Consumer{Username: "test-consumer", Plugins: configs}
	firstResolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "route-1"}))
	first, err := firstResolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("first resolveConsumerBindings() error = %v", err)
	}
	secondResolver := builder.resolveConsumerBindings(builder.pluginRouteContext(resource.Route{ID: "route-2"}))
	second, err := secondResolver(consumerResolutionRequest(consumer))
	if err != nil {
		t.Fatalf("second resolveConsumerBindings() error = %v", err)
	}
	if len(first.Bindings) != 1 || len(second.Bindings) != 1 {
		t.Fatalf("resolved bindings = %d/%d, want one per route", len(first.Bindings), len(second.Bindings))
	}
	if first.Bindings[0].Plugin == second.Bindings[0].Plugin {
		t.Fatal("consumer plugin instance shared across route identities")
	}
}
