package route

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
)

func registerExtraRoutes(mux *chi.Mux) {
	if pluginEnabled("node-status") {
		mux.Get("/apisix/status", node_status.StatusHandler)
		public_api.Register("GET", "/apisix/status", http.HandlerFunc(node_status.StatusHandler))
	}
	if pluginEnabled("server-info") {
		mux.Get("/v1/server_info", server_info.InfoHandler)
		public_api.Register("GET", "/v1/server_info", http.HandlerFunc(server_info.InfoHandler))
	}
	if pluginEnabled("batch-requests") {
		handler := batch_requests.NewHandler(mux)
		uri := batchRequestsURI()
		mux.Method("POST", uri, handler)
		public_api.Register("POST", batch_requests.DefaultURI, handler)
		if uri != batch_requests.DefaultURI {
			public_api.Register("POST", uri, handler)
		}
	}
}

func pluginEnabled(name string) bool {
	if config.GlobalConfig == nil {
		return false
	}
	for _, plugin := range config.GlobalConfig.Plugins {
		if plugin == name {
			return true
		}
	}
	return false
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
