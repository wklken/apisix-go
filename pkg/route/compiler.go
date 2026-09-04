package route

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

// PreparedRoute is the authority-free input to detached HTTP router assembly.
// Its handler may close over materialized bindings and prepared cluster
// handles, but route compilation cannot create or own either one.
type PreparedRoute struct {
	Route   resource.Route
	Hosts   []string
	Handler http.Handler
}

// CompileInput contains only owned values and already-prepared handlers.
type CompileInput struct {
	Revision                  uint64
	Routes                    []PreparedRoute
	NotFound                  http.Handler
	StaticConfig              *appconfig.Config
	Metadata                  runtime.MetadataView
	PublicAPIRegistry         *public_api.Registry
	GraphQLProxyCacheRegistry *graphql_proxy_cache.Registry
	ServerInfo                *server_info.View
}

// Snapshot is an immutable detached HTTP router snapshot.
type Snapshot struct {
	revision uint64
	handler  http.Handler
}

func (snapshot *Snapshot) Revision() uint64 {
	if snapshot == nil {
		return 0
	}
	return snapshot.revision
}

func (snapshot *Snapshot) Handler() http.Handler {
	if snapshot == nil {
		return nil
	}
	return snapshot.handler
}

// CompileHTTP assembles a detached router from already-prepared route
// handlers. It performs no Store lookup, plugin construction, resource
// acquisition, task startup, or production activation.
func CompileHTTP(ctx context.Context, input CompileInput) (*Snapshot, error) {
	if ctx == nil || input.Revision == 0 {
		return nil, fmt.Errorf("compile HTTP: context and revision are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	routes := make([]PreparedRoute, len(input.Routes))
	for index, supplied := range input.Routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if supplied.Route.ID == "" || supplied.Handler == nil {
			return nil, fmt.Errorf("compile HTTP: route id and handler are required")
		}
		if err := validateRouteCompatibility(supplied.Route); err != nil {
			return nil, fmt.Errorf("compile HTTP route %q: %w", supplied.Route.ID, err)
		}
		routes[index] = PreparedRoute{
			Route:   cloneCompileRoute(supplied.Route),
			Hosts:   slices.Clone(supplied.Hosts),
			Handler: supplied.Handler,
		}
	}
	slices.SortStableFunc(routes, func(left, right PreparedRoute) int {
		return cmp.Compare(left.Route.Priority, right.Route.Priority)
	})

	mux := chi.NewRouter()
	mux.Use(pinDecodedRoutePath)
	if input.NotFound == nil {
		input.NotFound = apisixRouteNotFoundHandler()
	}
	mux.MethodNotAllowed(input.NotFound.ServeHTTP)
	registrar := newRouteRegistrar(mux, input.NotFound)
	for _, prepared := range routes {
		if prepared.Route.Disabled() {
			continue
		}
		uris := prepared.Route.Uris
		if len(uris) == 0 && prepared.Route.Uri != "" {
			uris = []string{prepared.Route.Uri}
		}
		effectiveURIs := make(map[string]string, len(uris))
		for _, uri := range uris {
			converted, err := convertURI(uri)
			if err != nil {
				return nil, fmt.Errorf("compile HTTP route %q URI %q: %w", prepared.Route.ID, uri, err)
			}
			identity := effectiveRouteURI(converted)
			if previous, exists := effectiveURIs[identity]; exists {
				return nil, fmt.Errorf(
					"compile HTTP route %q: duplicate effective URI %q (from %q and %q)",
					prepared.Route.ID,
					identity,
					previous,
					uri,
				)
			}
			effectiveURIs[identity] = uri
		}
		hosts := prepared.Hosts
		if hosts == nil {
			hosts = prepared.Route.EffectiveHosts()
		}
		for _, uri := range uris {
			if err := registrar.registerRouteWithHosts(
				prepared.Route.Methods,
				uri,
				hosts,
				prepared.Handler,
			); err != nil {
				return nil, fmt.Errorf("compile HTTP route %q URI %q: %w", prepared.Route.ID, uri, err)
			}
		}
	}
	mux.NotFound(input.NotFound.ServeHTTP)
	if input.StaticConfig == nil && (input.PublicAPIRegistry != nil || input.GraphQLProxyCacheRegistry != nil) ||
		input.StaticConfig != nil && (input.PublicAPIRegistry == nil || input.GraphQLProxyCacheRegistry == nil) {
		return nil, fmt.Errorf("compile HTTP: static config and generation registries must be supplied together")
	}
	if input.StaticConfig != nil {
		if err := registerExtraRoutesStrict(
			mux,
			input.StaticConfig,
			input.Metadata,
			input.PublicAPIRegistry,
			input.GraphQLProxyCacheRegistry,
			input.ServerInfo,
		); err != nil {
			return nil, fmt.Errorf("compile HTTP extra routes: %w", err)
		}
	}
	return &Snapshot{revision: input.Revision, handler: mux}, nil
}

func cloneCompileRoute(source resource.Route) resource.Route {
	cloned := source
	cloned.Uris = slices.Clone(source.Uris)
	cloned.Methods = slices.Clone(source.Methods)
	cloned.Hosts = slices.Clone(source.Hosts)
	cloned.RemoteAddrs = slices.Clone(source.RemoteAddrs)
	cloned.Vars = slices.Clone(source.Vars)
	cloned.Script = slices.Clone(source.Script)
	cloned.ScriptID = slices.Clone(source.ScriptID)
	cloned.Plugins = cloneCompilePluginConfigs(source.Plugins)
	cloned.Labels = cloneCompileAnyMap(source.Labels)
	cloned.Upstream = cloneCompileUpstream(source.Upstream)
	return cloned
}

func cloneCompileUpstream(source resource.Upstream) resource.Upstream {
	cloned := source
	cloned.Nodes = slices.Clone(source.Nodes)
	cloned.Checks = cloneCompileAnyMap(source.Checks)
	if source.TLS != nil {
		tlsConfig := *source.TLS
		cloned.TLS = &tlsConfig
	}
	return cloned
}

func cloneCompilePluginConfigs(
	configs map[string]resource.PluginConfig,
) map[string]resource.PluginConfig {
	if configs == nil {
		return nil
	}
	cloned := make(map[string]resource.PluginConfig, len(configs))
	for name, config := range configs {
		cloned[name] = cloneCompileValue(config)
	}
	return cloned
}

func cloneCompileAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	cloned := maps.Clone(values)
	for name, value := range cloned {
		cloned[name] = cloneCompileValue(value)
	}
	return cloned
}

func cloneCompileValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCompileAnyMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for index, element := range typed {
			cloned[index] = cloneCompileValue(element)
		}
		return cloned
	case []string:
		return slices.Clone(typed)
	case []byte:
		return slices.Clone(typed)
	default:
		return value
	}
}

// validateRouteCompatibility is the single pre-materialization entrypoint for
// the documented Go data-plane route subset. It does not import the full
// APISIX 3.17 schema.
func validateRouteCompatibility(routeResource resource.Route) error {
	return validateRouteSemantics(routeResource)
}

func validateRouteSemantics(routeResource resource.Route) error {
	seenMethods := make(map[string]struct{}, len(routeResource.Methods))
	for _, method := range routeResource.Methods {
		if _, supported := supportedRouteMethods[method]; !supported {
			return fmt.Errorf("route %q method %q is unsupported by the Go data plane", routeResource.ID, method)
		}
		if _, duplicate := seenMethods[method]; duplicate {
			return fmt.Errorf("route %q method %q is duplicated", routeResource.ID, method)
		}
		seenMethods[method] = struct{}{}
	}
	if routeResource.HostConfigured() && routeResource.HostsConfigured() {
		return fmt.Errorf("route %q host and hosts cannot both be configured", routeResource.ID)
	}
	if routeResource.HostConfigured() && strings.TrimSpace(routeResource.Host) == "" {
		return fmt.Errorf("route %q host must not be empty", routeResource.ID)
	}
	if routeResource.HostsConfigured() && len(routeResource.EffectiveHosts()) == 0 {
		return fmt.Errorf("route %q hosts must not be empty", routeResource.ID)
	}
	for _, host := range routeResource.EffectiveHosts() {
		if err := validateRouteHost(host); err != nil {
			return fmt.Errorf("route %q host %q is invalid: %w", routeResource.ID, host, err)
		}
	}
	if routeResource.RemoteAddrConfigured() {
		return fmt.Errorf("route %q remote_addr is unsupported by the Go data plane", routeResource.ID)
	}
	if scriptID := bytes.TrimSpace(
		routeResource.ScriptID,
	); len(scriptID) > 0 &&
		!bytes.Equal(scriptID, []byte("null")) {
		return fmt.Errorf("route %q script_id is unsupported by the Go data plane", routeResource.ID)
	}
	if script := bytes.TrimSpace(routeResource.Script); len(script) > 0 && !bytes.Equal(script, []byte("null")) {
		return fmt.Errorf("route %q script is unsupported by the Go data plane", routeResource.ID)
	}
	if strings.TrimSpace(routeResource.FilterFunc) != "" {
		return fmt.Errorf("route %q filter_func is unsupported by the Go data plane", routeResource.ID)
	}
	if vars := bytes.TrimSpace(routeResource.Vars); len(vars) > 0 &&
		!bytes.Equal(vars, []byte("null")) &&
		!bytes.Equal(vars, []byte("[]")) {
		return fmt.Errorf("route %q vars is unsupported by the Go data plane", routeResource.ID)
	}
	for _, addr := range routeResource.RemoteAddrs {
		if strings.TrimSpace(addr) != "" {
			return fmt.Errorf("route %q remote_addrs is unsupported by the Go data plane", routeResource.ID)
		}
	}
	if routeResource.StatusConfigured() && routeResource.Status != 0 && routeResource.Status != 1 {
		return fmt.Errorf(
			"route %q status %d is unsupported by the Go data plane",
			routeResource.ID,
			routeResource.Status,
		)
	}
	return nil
}
