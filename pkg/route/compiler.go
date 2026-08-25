package route

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"
	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
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
	Revision          uint64
	Routes            []PreparedRoute
	NotFound          http.Handler
	StaticConfig      *appconfig.Config
	PublicAPIRegistry *public_api.Registry
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
	registrar := newRouteRegistrar(mux)
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
	if input.NotFound == nil {
		input.NotFound = http.NotFoundHandler()
	}
	mux.NotFound(input.NotFound.ServeHTTP)
	if (input.StaticConfig == nil) != (input.PublicAPIRegistry == nil) {
		return nil, fmt.Errorf("compile HTTP: static config and public API registry must be supplied together")
	}
	if input.StaticConfig != nil {
		if err := registerExtraRoutesStrict(mux, input.StaticConfig, input.PublicAPIRegistry); err != nil {
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
