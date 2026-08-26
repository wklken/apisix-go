package compiler

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

type httpPreparationPlan struct {
	resources         httpResourceSet
	plugins           *routepkg.HTTPPluginPlan
	enabledFactories  []string
	publicAPIRegistry *public_api.Registry
	purgeRegistry     *graphql_proxy_cache.Registry
	protoResolver     grpc_transcode.ProtoResolver
}

func (prepared *PreparedGeneration) planHTTPPreparation(
	ctx context.Context,
	candidate generation.PublicationCandidate,
) (*httpPreparationPlan, error) {
	if prepared == nil || ctx == nil || prepared.consumers == nil || prepared.effective == nil {
		return nil, fmt.Errorf("%w: HTTP preparation owner is incomplete", ErrInvalidInput)
	}
	owned, exists := prepared.attempt.Candidate(generation.DomainHTTP)
	if !exists || !reflect.DeepEqual(owned, candidate) {
		return nil, fmt.Errorf("%w: HTTP candidate is not owned by preparation attempt", ErrInvalidInput)
	}
	resources, err := decodeHTTPResourceSet(ctx, candidate)
	if err != nil {
		return nil, err
	}
	consumers := make(map[string]resource.Consumer, len(resources.consumerIDs))
	for _, id := range resources.consumerIDs {
		consumer, found := prepared.consumers.ConsumerByID(id)
		if !found {
			return nil, fmt.Errorf("%w: prepared HTTP consumer %q is missing", ErrInvalidInput, id)
		}
		consumers[id] = consumer
	}
	consumerGroups := make(map[string]resource.ConsumerGroup, len(resources.consumerGroupIDs))
	for _, id := range resources.consumerGroupIDs {
		group, found := prepared.consumers.ConsumerGroupByID(id)
		if !found {
			return nil, fmt.Errorf("%w: prepared HTTP consumer group %q is missing", ErrInvalidInput, id)
		}
		consumerGroups[id] = group
	}
	var dynamicPlugins []string
	if resources.dynamicPlugins {
		dynamicPlugins = make([]string, len(resources.enabledPlugins))
		copy(dynamicPlugins, resources.enabledPlugins)
	}
	plannedPlugins, err := routepkg.PlanHTTPPlugins(ctx, routepkg.PlanningInput{
		Routes: resources.routes, Services: resources.services,
		PluginConfigs: resources.pluginConfigs, GlobalRules: resources.globalRules,
		Consumers: consumers, ConsumerGroups: consumerGroups,
		EnabledPlugins: slices.Clone(prepared.effective.Config.Plugins),
		DynamicPlugins: dynamicPlugins, Profiles: prepared.effective.Profiles,
	})
	if err != nil {
		return nil, err
	}
	enabledFactories := slices.Clone(prepared.effective.Config.Plugins)
	if resources.dynamicPlugins {
		enabledFactories = make([]string, len(resources.enabledPlugins))
		copy(enabledFactories, resources.enabledPlugins)
	}
	registry := public_api.NewRegistry()
	if err := routepkg.SeedPublicAPIRegistry(registry, &prepared.effective.Config); err != nil {
		return nil, err
	}
	return &httpPreparationPlan{
		resources: resources, plugins: plannedPlugins,
		enabledFactories:  enabledFactories,
		publicAPIRegistry: registry,
		purgeRegistry:     graphql_proxy_cache.NewRegistry(),
		protoResolver:     newHTTPProtoResolver(resources.protos),
	}, nil
}

func newHTTPProtoResolver(source map[string]string) grpc_transcode.ProtoResolver {
	protos := maps.Clone(source)
	return func(id string) (string, error) {
		content, exists := protos[id]
		if !exists {
			return "", fmt.Errorf("proto %q is missing", id)
		}
		return content, nil
	}
}
