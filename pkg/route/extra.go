package route

import (
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
	"github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	prometheusplugin "github.com/wklken/apisix-go/pkg/plugin/prometheus"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var registerPurgeMethodOnce sync.Once

func registerPurgeMethod() {
	registerPurgeMethodOnce.Do(func() {
		chi.RegisterMethod("PURGE")
	})
}

func registerExtraRoutes(mux *chi.Mux, staticConfig *config.Config, registries ...*public_api.Registry) {
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	_ = registerExtraRoutesStrict(
		mux,
		staticConfig,
		runtime.MetadataView{},
		registry,
		graphql_proxy_cache.NewRegistry(),
		nil,
	)
}

func registerExtraRoutesStrict(
	mux *chi.Mux,
	staticConfig *config.Config,
	metadata runtime.MetadataView,
	registry *public_api.Registry,
	graphqlPurgeRegistry *graphql_proxy_cache.Registry,
	serverInfoView *server_info.View,
) error {
	if registry == nil {
		registry = public_api.NewRegistry()
	}
	if pluginEnabled(staticConfig, "node-status") {
		mux.Handle("/apisix/status", http.NotFoundHandler())
		handler := node_status.StatusHandler(staticConfig.Apisix.ID)
		mux.Get("/apisix/status", handler)
		registry.Register("GET", "/apisix/status", handler)
	}
	if pluginEnabled(staticConfig, "server-info") {
		if serverInfoView == nil {
			serverInfoView = server_info.NewView(staticConfig.Apisix.ID)
		}
		handler := serverInfoView.Handler()
		registry.Register("GET", "/v1/server_info", handler)
	}
	if pluginEnabled(staticConfig, "batch-requests") {
		handler, err := batch_requests.NewHandlerFromMetadata(mux, metadata)
		if err != nil {
			return fmt.Errorf("configure batch-requests endpoint: %w", err)
		}
		uri := batchRequestsURI(staticConfig)
		mux.Method("POST", uri, handler)
		registry.Register("POST", batch_requests.DefaultURI, handler)
		if uri != batch_requests.DefaultURI {
			registry.Register("POST", uri, handler)
		}
	}
	if pluginEnabled(staticConfig, "graphql-proxy-cache") {
		if graphqlPurgeRegistry == nil {
			return fmt.Errorf("graphql proxy cache purge registry is required")
		}
		registerPurgeMethod()
		mux.Method("PURGE", graphql_proxy_cache.PurgeURI, graphqlPurgeRegistry.PurgeHandler())
	}
	return nil
}

func registerPrometheusPublicEndpoint(registry *public_api.Registry, staticConfig *config.Config) error {
	if !pluginEnabled(staticConfig, "prometheus") {
		return nil
	}
	endpoint, err := metrics.ConfiguredPublicEndpoint(staticConfig.PluginAttr["prometheus"])
	if err != nil {
		return fmt.Errorf("configure prometheus public endpoint: %w", err)
	}
	if !endpoint.Enabled {
		registry.Register(http.MethodGet, endpoint.URI, http.HandlerFunc(prometheusplugin.MetricsHandler))
	}
	return nil
}

// SeedPublicAPIRegistry installs generation-wide public endpoints before any
// route plugin is materialized. Later route/plugin registrations retain the
// legacy last-writer ordering within this generation-local registry.
func SeedPublicAPIRegistry(registry *public_api.Registry, staticConfig *config.Config) error {
	if registry == nil || staticConfig == nil {
		return fmt.Errorf("seed public API registry: registry and static config are required")
	}
	return registerPrometheusPublicEndpoint(registry, staticConfig)
}

func pluginEnabled(staticConfig *config.Config, name string) bool {
	if staticConfig == nil {
		return false
	}
	return slices.Contains(staticConfig.Plugins, name)
}

func batchRequestsURI(staticConfig *config.Config) string {
	if staticConfig == nil {
		return batch_requests.DefaultURI
	}
	attr := staticConfig.PluginAttr["batch-requests"]
	if attr == nil {
		return batch_requests.DefaultURI
	}
	uri, ok := attr["uri"].(string)
	if !ok || uri == "" {
		return batch_requests.DefaultURI
	}
	return uri
}
