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

func registerExtraRoutes(mux *chi.Mux, registries ...*public_api.Registry) {
	_ = registerExtraRoutesStrict(mux, registries...)
}

func registerExtraRoutesStrict(mux *chi.Mux, registries ...*public_api.Registry) error {
	registry := public_api.NewRegistry()
	if len(registries) > 0 && registries[0] != nil {
		registry = registries[0]
	}
	if pluginEnabled("node-status") {
		mux.Handle("/apisix/status", http.NotFoundHandler())
		mux.Get("/apisix/status", node_status.StatusHandler)
		registry.Register("GET", "/apisix/status", http.HandlerFunc(node_status.StatusHandler))
	}
	if pluginEnabled("server-info") {
		mux.Get("/v1/server_info", server_info.InfoHandler)
		registry.Register("GET", "/v1/server_info", http.HandlerFunc(server_info.InfoHandler))
	}
	if pluginEnabled("batch-requests") {
		handler := batch_requests.NewHandler(mux)
		uri := batchRequestsURI()
		mux.Method("POST", uri, handler)
		registry.Register("POST", batch_requests.DefaultURI, handler)
		if uri != batch_requests.DefaultURI {
			registry.Register("POST", uri, handler)
		}
	}
	if pluginEnabled("graphql-proxy-cache") {
		registerPurgeMethod()
		mux.Method("PURGE", graphql_proxy_cache.PurgeURI, http.HandlerFunc(graphql_proxy_cache.PurgeHandler))
	}
	return nil
}

func registerPrometheusPublicEndpoint(registry *public_api.Registry) error {
	if !pluginEnabled("prometheus") {
		return nil
	}
	endpoint, err := metrics.ConfiguredPublicEndpoint()
	if err != nil {
		return fmt.Errorf("configure prometheus public endpoint: %w", err)
	}
	if !endpoint.Enabled {
		registry.Register(http.MethodGet, endpoint.URI, http.HandlerFunc(prometheusplugin.MetricsHandler))
	}
	return nil
}

func pluginEnabled(name string) bool {
	if config.GlobalConfig == nil {
		return false
	}
	return slices.Contains(config.GlobalConfig.Plugins, name)
}

func batchRequestsURI() string {
	if config.GlobalConfig == nil {
		return batch_requests.DefaultURI
	}
	attr := config.GlobalConfig.PluginAttr["batch-requests"]
	if attr == nil {
		return batch_requests.DefaultURI
	}
	uri, ok := attr["uri"].(string)
	if !ok || uri == "" {
		return batch_requests.DefaultURI
	}
	return uri
}
