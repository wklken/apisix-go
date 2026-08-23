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
)

var registerPurgeMethodOnce sync.Once

func registerPurgeMethod() {
	registerPurgeMethodOnce.Do(func() {
		chi.RegisterMethod("PURGE")
	})
}

func registerExtraRoutes(mux *chi.Mux, staticConfig *config.Config, registries ...*public_api.Registry) {
	_ = registerExtraRoutesStrict(mux, staticConfig, registries...)
}

func registerExtraRoutesStrict(
	mux *chi.Mux,
	staticConfig *config.Config,
	registries ...*public_api.Registry,
) error {
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	if pluginEnabled(staticConfig, "node-status") {
		mux.Handle("/apisix/status", http.NotFoundHandler())
		handler := node_status.StatusHandler(staticConfig.Apisix.ID)
		mux.Get("/apisix/status", handler)
		registry.Register("GET", "/apisix/status", handler)
	}
	if pluginEnabled(staticConfig, "server-info") {
		handler := server_info.InfoHandler(staticConfig.Apisix.ID)
		mux.Get("/v1/server_info", handler)
		registry.Register("GET", "/v1/server_info", handler)
	}
	if pluginEnabled(staticConfig, "batch-requests") {
		handler := batch_requests.NewHandler(mux)
		uri := batchRequestsURI(staticConfig)
		mux.Method("POST", uri, handler)
		registry.Register("POST", batch_requests.DefaultURI, handler)
		if uri != batch_requests.DefaultURI {
			registry.Register("POST", uri, handler)
		}
	}
	if pluginEnabled(staticConfig, "graphql-proxy-cache") {
		registerPurgeMethod()
		mux.Method("PURGE", graphql_proxy_cache.PurgeURI, http.HandlerFunc(graphql_proxy_cache.PurgeHandler))
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
