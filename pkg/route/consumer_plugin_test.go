package route

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestConsumerPluginChainDoesNotShareRouteBoundPluginsAcrossRoutes(t *testing.T) {
	config := map[string]any{
		"headers": map[string]any{
			"set": map[string]any{"X-Route-Plugin": "isolated"},
		},
	}

	first := testPluginBinding(t, "proxy-rewrite", config, resource.Route{ID: "route-1"})
	second := testPluginBinding(t, "proxy-rewrite", config, resource.Route{ID: "route-2"})
	if first.Plugin == second.Plugin {
		t.Fatal("consumer plugin instance shared across route identities")
	}
}
