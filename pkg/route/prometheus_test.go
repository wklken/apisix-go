package route

import (
	"testing"

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
