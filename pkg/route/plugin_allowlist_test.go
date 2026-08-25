package route

import (
	"context"
	"os"
	"strings"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestHTTPPluginAllowlist(t *testing.T) {
	for _, test := range []struct {
		name       string
		resourceID string
		kind       plugin.ResourceKind
	}{
		{name: "route", resourceID: "allowlist-route", kind: plugin.ResourceRoute},
		{name: "plugin-config", resourceID: "allowlist-plugin-config", kind: plugin.ResourcePluginConfig},
		{name: "service", resourceID: "allowlist-service", kind: plugin.ResourceService},
		{name: "global-rule", resourceID: "allowlist-global-rule", kind: plugin.ResourceGlobalRule},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := planPluginSources([]materializedPluginSource{{
				name: "request-id", config: map[string]any{}, scope: plugin.ScopeRoute,
				provenance: plugin.ResourceProvenance{Kind: test.kind, ID: test.resourceID},
			}}, plugin.NewEnabledSet(nil), false)
			if err == nil {
				t.Fatal("planPluginSources() error = nil, want disabled-plugin rejection")
			}
			for _, want := range []string{"request-id", string(test.kind), test.resourceID} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("planPluginSources() error = %q, want %q", err, want)
				}
			}
		})
	}

	t.Run("enabled controls build", func(t *testing.T) {
		plans, err := planPluginSources([]materializedPluginSource{{
			name: "request-id", config: map[string]any{}, scope: plugin.ScopeRoute,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "allowlist-enabled-route"},
		}}, plugin.NewEnabledSet([]string{"request-id"}), false)
		if err != nil || len(plans) != 1 {
			t.Fatalf("planPluginSources() = (%d, %v), want enabled route plan", len(plans), err)
		}
	})

	t.Run("strict empty still builds system request context", func(t *testing.T) {
		plans, err := planPluginSources([]materializedPluginSource{{
			name: "request-context", config: map[string]any{}, scope: plugin.ScopeSystem,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
		}}, plugin.NewEnabledSet(nil), true)
		if err != nil || len(plans) != 1 {
			t.Fatalf("planPluginSources() = (%d, %v), want system request-context bypass", len(plans), err)
		}
	})

	t.Run("user request context requires membership", func(t *testing.T) {
		_, err := planPluginSources([]materializedPluginSource{{
			name: "request-context", config: map[string]any{}, scope: plugin.ScopeRoute,
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "allowlist-user-request-context"},
		}}, plugin.NewEnabledSet(nil), false)
		if !strings.Contains(err.Error(), "request-context") {
			t.Fatalf("planPluginSources() error = %q, want request-context", err)
		}
	})

	t.Run("metadata disable does not bypass membership", func(t *testing.T) {
		_, err := planPluginSources([]materializedPluginSource{{
			name: "request-id", config: map[string]any{"_meta": map[string]any{"disable": true}},
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "allowlist-meta-disabled"},
		}}, plugin.NewEnabledSet(nil), false)
		if !strings.Contains(err.Error(), "request-id") {
			t.Fatalf("planPluginSources() error = %q, want request-id", err)
		}
	})
}

func TestDisabledMCPBridge(t *testing.T) {
	marker := t.TempDir() + "/mcp-started"
	_, err := planPluginSources([]materializedPluginSource{{
		name: "mcp-bridge",
		config: map[string]any{
			"command": "/bin/sh", "args": []any{"-c", "printf started > " + marker},
		},
		provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "allowlist-disabled-mcp"},
	}}, plugin.NewEnabledSet(nil), false)
	if err == nil {
		t.Fatal("planPluginSources() error = nil, want disabled mcp-bridge rejection")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("mcp marker stat error = %v, command may have started", statErr)
	}
}

func TestHTTPPluginAllowlistConsumerPlugin(t *testing.T) {
	t.Run("disabled consumer plugin fails closed", func(t *testing.T) {
		marker := t.TempDir() + "/consumer-mcp-started"
		consumer := resource.Consumer{
			Username: "allowlist-consumer",
			Plugins: map[string]resource.PluginConfig{
				"mcp-bridge": map[string]any{
					"command": "/bin/sh",
					"args":    []any{"-c", "printf started > " + marker},
				},
			},
		}
		_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			Consumers: map[string]resource.Consumer{consumer.Username: consumer},
		})
		if err == nil || !strings.Contains(err.Error(), "mcp-bridge") {
			t.Fatalf("PlanHTTPPlugins() error = %v, want disabled consumer plugin", err)
		}
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("consumer mcp marker stat error = %v, command may have started", statErr)
		}
	})

	t.Run("disabled group plugin fails closed", func(t *testing.T) {
		const groupID = "allowlist-consumer-group"
		consumer := resource.Consumer{Username: "allowlist-group-consumer", GroupID: groupID}
		_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			Consumers: map[string]resource.Consumer{consumer.Username: consumer},
			ConsumerGroups: map[string]resource.ConsumerGroup{groupID: {
				Plugins: map[string]resource.PluginConfig{
					"mcp-bridge": map[string]any{"command": "/bin/true"},
				},
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "mcp-bridge") {
			t.Fatalf("PlanHTTPPlugins() error = %v, want disabled consumer-group plugin", err)
		}
	})
}

func httpPluginAllowlist(names ...string) *appconfig.EffectiveConfig {
	effective := testEffectiveConfig()
	effective.Config.Plugins = append([]string(nil), names...)
	return effective
}
