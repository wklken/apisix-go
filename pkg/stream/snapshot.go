package stream

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

// PreparedRoute is one fully resolved stream route and its optional,
// generation-owned protocol binding. A zero Protocol binding denotes raw TCP.
type PreparedRoute struct {
	Route    resource.StreamRoute
	Protocol plugin.Binding
}

// CompileInput contains only detached stream-router construction inputs.
type CompileInput struct {
	Revision uint64
	Routes   []PreparedRoute
	OnResult func(Result)
}

// CompileRouter constructs a detached immutable router. It neither opens a
// listener nor constructs, initializes, or materializes a plugin instance.
func CompileRouter(ctx context.Context, input CompileInput) (*Router, error) {
	if ctx == nil {
		return nil, fmt.Errorf("compile stream router: context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input.Revision == 0 {
		return nil, fmt.Errorf("compile stream router: revision is required")
	}

	prepared := make([]PreparedRoute, len(input.Routes))
	routes := make([]resource.StreamRoute, len(input.Routes))
	for index := range input.Routes {
		prepared[index] = PreparedRoute{
			Route:    cloneDetachedStreamRoute(input.Routes[index].Route),
			Protocol: cloneDetachedBinding(input.Routes[index].Protocol),
		}
		routes[index] = prepared[index].Route
	}
	if err := rejectConflictingStreamListens(routes); err != nil {
		return nil, err
	}

	entries := make([]routeEntry, 0, len(prepared))
	for index := range prepared {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := compilePreparedRoute(prepared[index])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := orderStreamRoutes(entries); err != nil {
		return nil, err
	}
	return &Router{
		routes:   entries,
		onResult: input.OnResult,
	}, nil
}

// RouteIDs returns a detached copy of the compiled route order.
func (r *Router) RouteIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, len(r.routes))
	for index := range r.routes {
		ids[index] = r.routes[index].route.ID
	}
	return ids
}

func compilePreparedRoute(prepared PreparedRoute) (routeEntry, error) {
	route := prepared.Route
	binding := prepared.Protocol
	switch len(route.Plugins) {
	case 0:
		if detachedBindingSupplied(binding) {
			return routeEntry{}, fmt.Errorf(
				"stream route %q raw TCP route cannot carry a protocol binding",
				route.ID,
			)
		}
		route.Plugins = nil
		entry, err := buildRouteEntryBase(route)
		if err != nil {
			return routeEntry{}, err
		}
		entry.serve = entry.rawServe
		return entry, nil
	case 1:
	default:
		return routeEntry{}, fmt.Errorf(
			"stream route %q must configure exactly one effective stream plugin",
			route.ID,
		)
	}

	factory := ""
	for name := range route.Plugins {
		factory = name
	}
	if factory != "mqtt-proxy" {
		return routeEntry{}, fmt.Errorf(
			"stream plugin %q is not supported by the detached Go stream owner",
			factory,
		)
	}
	if !detachedBindingSupplied(binding) {
		return routeEntry{}, fmt.Errorf(
			"stream route %q requires one prepared mqtt-proxy binding",
			route.ID,
		)
	}
	p, err := validateDetachedMQTTBinding(route, binding)
	if err != nil {
		return routeEntry{}, err
	}

	// Raw configuration documents belong to the compiler/materializer and must
	// not remain observable from the serving entry.
	route.Plugins = nil
	entry, err := buildRouteEntryBase(route)
	if err != nil {
		return routeEntry{}, err
	}
	bindMQTTProtocol(&entry, p)
	return entry, nil
}

func validateDetachedMQTTBinding(
	route resource.StreamRoute,
	binding plugin.Binding,
) (*mqtt_proxy.Plugin, error) {
	const factory = "mqtt-proxy"
	if binding.Descriptor.Factory != factory {
		return nil, fmt.Errorf(
			"stream route %q protocol binding descriptor factory = %q, want %q",
			route.ID,
			binding.Descriptor.Factory,
			factory,
		)
	}
	if binding.Descriptor.Implementation != factory ||
		!slices.Equal(binding.Descriptor.Phases, []plugin.Phase{plugin.PhaseProtocol}) ||
		!slices.Contains(binding.Descriptor.Scopes, plugin.ScopeRoute) {
		return nil, fmt.Errorf(
			"stream route %q protocol binding has an incomplete mqtt-proxy descriptor",
			route.ID,
		)
	}
	if binding.Scope != plugin.ScopeRoute {
		return nil, fmt.Errorf(
			"stream route %q protocol binding scope = %d, want route scope",
			route.ID,
			binding.Scope,
		)
	}
	if !validDetachedProtocolProvenance(route, binding.Provenance) {
		return nil, fmt.Errorf(
			"stream route %q protocol binding provenance = %s/%s, want its route or referenced service",
			route.ID,
			binding.Provenance.Kind,
			binding.Provenance.ID,
		)
	}
	key := binding.InstanceKey
	if key.Factory != factory || key.Attempt == (plugin.InstanceKey{}.Attempt) ||
		key.Scope != binding.Scope || key.Owner != binding.Provenance {
		return nil, fmt.Errorf(
			"stream route %q protocol binding instance owner does not match its materialized provenance",
			route.ID,
		)
	}
	if binding.Priority != binding.Descriptor.Priority {
		return nil, fmt.Errorf(
			"stream route %q protocol binding priority = %d, want descriptor priority %d",
			route.ID,
			binding.Priority,
			binding.Descriptor.Priority,
		)
	}
	p, ok := binding.Plugin.(*mqtt_proxy.Plugin)
	if !ok || p == nil || p.GetName() != factory || p.GetPriority() != binding.Priority {
		return nil, fmt.Errorf(
			"stream route %q protocol binding does not own a materialized mqtt-proxy instance",
			route.ID,
		)
	}
	return p, nil
}

func validDetachedProtocolProvenance(
	route resource.StreamRoute,
	provenance plugin.ResourceProvenance,
) bool {
	switch provenance.Kind {
	case plugin.ResourceRoute:
		return provenance.ID != "" && provenance.ID == route.ID
	case plugin.ResourceService:
		return provenance.ID != "" && provenance.ID == route.ServiceID
	default:
		return false
	}
}

func detachedBindingSupplied(binding plugin.Binding) bool {
	return binding.Plugin != nil ||
		binding.Descriptor.Factory != "" ||
		binding.Descriptor.Implementation != "" ||
		len(binding.Descriptor.Phases) != 0 ||
		binding.Descriptor.Priority != 0 ||
		len(binding.Descriptor.Scopes) != 0 ||
		binding.Descriptor.InstanceScope != "" ||
		binding.Priority != 0 ||
		binding.Scope != plugin.ScopeSystem ||
		binding.Provenance != (plugin.ResourceProvenance{}) ||
		binding.InstanceKey != (plugin.InstanceKey{})
}

func cloneDetachedBinding(binding plugin.Binding) plugin.Binding {
	binding.Descriptor.Phases = append([]plugin.Phase(nil), binding.Descriptor.Phases...)
	binding.Descriptor.Scopes = append([]plugin.Scope(nil), binding.Descriptor.Scopes...)
	return binding
}

func cloneDetachedStreamRoute(route resource.StreamRoute) resource.StreamRoute {
	route.Plugins = cloneDetachedPluginConfigs(route.Plugins)
	route.Upstream = cloneDetachedUpstream(route.Upstream)
	return route
}

func cloneDetachedUpstream(upstream resource.Upstream) resource.Upstream {
	upstream.Nodes = append([]resource.Node(nil), upstream.Nodes...)
	upstream.Checks = cloneDetachedAnyMap(upstream.Checks)
	if upstream.TLS != nil {
		tls := *upstream.TLS
		tls.ClientCertID = cloneDetachedAny(tls.ClientCertID)
		upstream.TLS = &tls
	}
	return upstream
}

func cloneDetachedPluginConfigs(
	source map[string]resource.PluginConfig,
) map[string]resource.PluginConfig {
	if source == nil {
		return nil
	}
	cloned := make(map[string]resource.PluginConfig, len(source))
	for name, config := range source {
		cloned[name] = cloneDetachedAny(config)
	}
	return cloned
}

func cloneDetachedAnyMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneDetachedAny(value)
	}
	return cloned
}

func cloneDetachedAny(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneDetachedAnyMap(value)
	case []any:
		cloned := make([]any, len(value))
		for index := range value {
			cloned[index] = cloneDetachedAny(value[index])
		}
		return cloned
	case []string:
		return append([]string(nil), value...)
	case map[string]string:
		cloned := make(map[string]string, len(value))
		maps.Copy(cloned, value)
		return cloned
	default:
		return value
	}
}
