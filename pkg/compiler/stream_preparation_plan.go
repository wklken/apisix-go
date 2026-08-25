package compiler

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

type streamBindingRequest struct {
	factory    string
	config     resource.PluginConfig
	source     generation.ResourceKey
	scope      plugin.Scope
	provenance plugin.ResourceProvenance
}

type plannedStreamRoute struct {
	route   resource.StreamRoute
	service resource.Service
	binding *streamBindingRequest
}

type streamPreparationPlan struct {
	revision         uint64
	routes           []plannedStreamRoute
	enabledFactories []string
}

func (prepared *PreparedGeneration) planStreamPreparation(
	ctx context.Context,
	candidate generation.PublicationCandidate,
) (*streamPreparationPlan, error) {
	if prepared == nil || ctx == nil || prepared.effective == nil || prepared.manifest == nil {
		return nil, fmt.Errorf("%w: stream preparation owner is incomplete", ErrInvalidInput)
	}
	owned, exists := prepared.attempt.Candidate(generation.DomainStream)
	if !exists || !reflect.DeepEqual(owned, candidate) {
		return nil, fmt.Errorf("%w: stream candidate is not owned by preparation attempt", ErrInvalidInput)
	}
	resources, err := decodeStreamResourceSet(ctx, candidate)
	if err != nil {
		return nil, err
	}
	return buildStreamPreparationPlan(
		ctx,
		resources,
		prepared.effective.Config.StreamPlugins,
		prepared.manifest,
	)
}

func buildStreamPreparationPlan(
	ctx context.Context,
	resources streamResourceSet,
	staticEnabled []string,
	manifest *capability.Manifest,
) (*streamPreparationPlan, error) {
	if ctx == nil || manifest == nil {
		return nil, fmt.Errorf("%w: stream planning input is incomplete", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	enabledFactories := slices.Clone(staticEnabled)
	if resources.dynamicPlugins {
		enabledFactories = slices.Clone(resources.enabledPlugins)
		if enabledFactories == nil {
			enabledFactories = make([]string, 0)
		}
	}
	enabled := plugin.NewEnabledSet(enabledFactories)
	plan := &streamPreparationPlan{
		revision:         resources.revision,
		routes:           make([]plannedStreamRoute, 0, len(resources.routes)),
		enabledFactories: enabledFactories,
	}
	seenIDs := make(map[string]struct{}, len(resources.routes))
	seenListens := make(map[string]string, len(resources.routes))
	for _, supplied := range resources.routes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		planned, err := planOneStreamRoute(supplied, resources, enabled, manifest)
		if err != nil {
			return nil, err
		}
		if planned.route.ID == "" {
			return nil, fmt.Errorf("%w: stream route id is required", ErrInvalidInput)
		}
		if _, duplicate := seenIDs[planned.route.ID]; duplicate {
			return nil, fmt.Errorf("%w: stream route id %q is duplicated", ErrInvalidInput, planned.route.ID)
		}
		seenIDs[planned.route.ID] = struct{}{}
		if planned.route.ServerAddr != "" || planned.route.ServerPort != 0 {
			listen := planned.route.ServerAddr + "\x00" + strconv.Itoa(planned.route.ServerPort)
			if previous, conflict := seenListens[listen]; conflict {
				return nil, fmt.Errorf(
					"conflicting stream listen address %s:%d between %q and %q",
					planned.route.ServerAddr,
					planned.route.ServerPort,
					previous,
					planned.route.ID,
				)
			}
			seenListens[listen] = planned.route.ID
		}
		plan.routes = append(plan.routes, planned)
	}
	return plan, nil
}

type streamPluginSource struct {
	config     resource.PluginConfig
	source     generation.ResourceKey
	provenance plugin.ResourceProvenance
}

func planOneStreamRoute(
	supplied resource.StreamRoute,
	resources streamResourceSet,
	enabled plugin.EnabledSet,
	manifest *capability.Manifest,
) (plannedStreamRoute, error) {
	route, err := cloneEffectiveStreamRoute(supplied)
	if err != nil {
		return plannedStreamRoute{}, fmt.Errorf("clone stream route %q: %w", supplied.ID, err)
	}
	if route.ID == "" {
		return plannedStreamRoute{}, fmt.Errorf("%w: stream route id is required", ErrInvalidInput)
	}
	var service resource.Service
	pluginSources := make(map[string]streamPluginSource)
	if route.ServiceID != "" && route.UpstreamID == "" {
		suppliedService, exists := resources.services[route.ServiceID]
		if !exists {
			return plannedStreamRoute{}, fmt.Errorf(
				"stream route %q references missing service %q",
				route.ID,
				route.ServiceID,
			)
		}
		service, err = cloneEffectiveService(suppliedService)
		if err != nil {
			return plannedStreamRoute{}, fmt.Errorf("clone stream service %q: %w", route.ServiceID, err)
		}
		for name, config := range service.Plugins {
			pluginSources[name] = streamPluginSource{
				config:     config,
				source:     generation.ResourceKey{Kind: "services", ID: route.ServiceID},
				provenance: plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: route.ServiceID},
			}
		}
		if len(route.Upstream.Nodes) == 0 {
			route.Upstream = service.Upstream
			route.UpstreamID = service.UpstreamID
		}
	}
	for name, config := range route.Plugins {
		pluginSources[name] = streamPluginSource{
			config:     config,
			source:     generation.ResourceKey{Kind: "stream_routes", ID: route.ID},
			provenance: plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID},
		}
	}
	if route.UpstreamID != "" && len(route.Upstream.Nodes) == 0 {
		upstream, exists := resources.upstreams[route.UpstreamID]
		if !exists {
			return plannedStreamRoute{}, fmt.Errorf(
				"stream route %q references missing upstream %q",
				route.ID,
				route.UpstreamID,
			)
		}
		route.Upstream, err = cloneEffectiveUpstream(upstream)
		if err != nil {
			return plannedStreamRoute{}, fmt.Errorf("clone stream upstream %q: %w", route.UpstreamID, err)
		}
	}
	if err := validatePlannedStreamUpstream(route); err != nil {
		return plannedStreamRoute{}, err
	}

	names := make([]string, 0, len(pluginSources))
	configs := make(map[string]resource.PluginConfig, len(pluginSources))
	for name, source := range pluginSources {
		config, disabled, err := normalizeStreamPluginConfig(source.config)
		if err != nil {
			return plannedStreamRoute{}, fmt.Errorf(
				"stream plugin %q from %s/%s: %w",
				name,
				source.source.Kind,
				source.source.ID,
				err,
			)
		}
		if disabled {
			continue
		}
		names = append(names, name)
		configs[name] = config
		pluginSources[name] = streamPluginSource{
			config:     config,
			source:     source.source,
			provenance: source.provenance,
		}
	}
	slices.Sort(names)
	if len(names) > 1 {
		return plannedStreamRoute{}, fmt.Errorf(
			"stream route %q has more than one effective protocol plugin: %v",
			route.ID,
			names,
		)
	}
	route.Plugins = nil
	planned := plannedStreamRoute{route: route, service: service}
	if len(names) == 0 {
		return planned, nil
	}
	factory := names[0]
	if !supportedStreamFactory(manifest, factory) || factory != "mqtt-proxy" {
		return plannedStreamRoute{}, fmt.Errorf(
			"stream factory %q is not supported by the Go stream owner",
			factory,
		)
	}
	if !enabled.Contains(factory) {
		return plannedStreamRoute{}, fmt.Errorf("stream plugin %q is not enabled", factory)
	}
	source := pluginSources[factory]
	config := configs[factory]
	route.Plugins = map[string]resource.PluginConfig{factory: config}
	planned.route = route
	planned.binding = &streamBindingRequest{
		factory:    factory,
		config:     config,
		source:     source.source,
		scope:      plugin.ScopeRoute,
		provenance: source.provenance,
	}
	return planned, nil
}

func supportedStreamFactory(manifest *capability.Manifest, factory string) bool {
	if manifest == nil || factory == "" {
		return false
	}
	for _, pluginCapability := range manifest.Plugins {
		if !slices.Contains(pluginCapability.Domains, capability.DomainStream) ||
			!slices.Contains(pluginCapability.Phases, "protocol") {
			continue
		}
		for _, candidate := range pluginCapability.Factories {
			if candidate.Key == factory {
				return true
			}
		}
	}
	return false
}

func normalizeStreamPluginConfig(config resource.PluginConfig) (resource.PluginConfig, bool, error) {
	owned, err := cloneEffectiveBindingValue(config)
	if err != nil {
		return nil, false, err
	}
	values, ok := owned.(map[string]any)
	if !ok {
		return owned, false, nil
	}
	rawMeta, exists := values["_meta"]
	if !exists {
		return values, false, nil
	}
	metadata, ok := rawMeta.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("_meta must be an object")
	}
	disabled := false
	if rawDisable, exists := metadata["disable"]; exists {
		var valid bool
		disabled, valid = rawDisable.(bool)
		if !valid {
			return nil, false, fmt.Errorf("_meta.disable must be a boolean")
		}
	}
	delete(values, "_meta")
	return values, disabled, nil
}

func validatePlannedStreamUpstream(route resource.StreamRoute) error {
	upstream := route.Upstream
	if upstream.DiscoveryType != "" || upstream.ServiceName != "" {
		return fmt.Errorf(
			"stream route %q upstream discovery is not supported",
			route.ID,
		)
	}
	if upstream.TLS != nil {
		return fmt.Errorf("stream route %q upstream TLS is not supported", route.ID)
	}
	if upstream.Scheme != "" && upstream.Scheme != "tcp" {
		return fmt.Errorf("unsupported stream upstream scheme %q", upstream.Scheme)
	}
	if strings.EqualFold(upstream.Type, "chash") && upstream.HashOn != "" &&
		!strings.EqualFold(upstream.HashOn, "vars") {
		return fmt.Errorf("unsupported stream chash hash_on %q", upstream.HashOn)
	}
	if route.RemoteAddr != "" && net.ParseIP(route.RemoteAddr) == nil {
		if _, _, err := net.ParseCIDR(route.RemoteAddr); err != nil {
			return fmt.Errorf("stream route %q remote_addr %q is invalid", route.ID, route.RemoteAddr)
		}
	}
	if len(upstream.Nodes) == 0 {
		if route.UpstreamID != "" {
			return fmt.Errorf("stream route %q upstream_id %q was not resolved", route.ID, route.UpstreamID)
		}
		return fmt.Errorf("stream route %q has no upstream nodes", route.ID)
	}
	positive := false
	for _, node := range upstream.Nodes {
		host := strings.TrimSpace(node.Host)
		port := node.Port
		if host == "" {
			return fmt.Errorf("stream route %q upstream node host is empty", route.ID)
		}
		if _, parsedPort, err := net.SplitHostPort(host); err == nil {
			parsed, parseErr := strconv.Atoi(parsedPort)
			if parseErr != nil {
				return fmt.Errorf("stream route %q upstream node port %q is invalid", route.ID, parsedPort)
			}
			port = parsed
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("stream route %q upstream node port %d is invalid", route.ID, port)
		}
		weight := node.Weight
		if !node.WeightConfigured() {
			weight = 1
		}
		if weight < 0 {
			return fmt.Errorf("stream route %q upstream node weight must be non-negative", route.ID)
		}
		positive = positive || weight > 0
	}
	if !positive {
		return fmt.Errorf(
			"stream route %q upstream node weights: at least one upstream node must have a positive weight",
			route.ID,
		)
	}
	return nil
}
