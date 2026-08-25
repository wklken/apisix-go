package compiler

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

type httpPreparationPlan struct {
	resources         httpResourceSet
	plugins           *routepkg.HTTPPluginPlan
	publicAPIRegistry *public_api.Registry
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
		dynamicPlugins = slices.Clone(resources.enabledPlugins)
	}
	plannedPlugins, err := routepkg.PlanHTTPPlugins(ctx, routepkg.PlanningInput{
		Routes: resources.routes, Services: resources.services,
		PluginConfigs: resources.pluginConfigs, GlobalRules: resources.globalRules,
		Consumers: consumers, ConsumerGroups: consumerGroups,
		EnabledPlugins: slices.Clone(prepared.effective.Config.Plugins),
		DynamicPlugins: dynamicPlugins,
	})
	if err != nil {
		return nil, err
	}
	return &httpPreparationPlan{
		resources: resources, plugins: plannedPlugins,
		publicAPIRegistry: public_api.NewRegistry(),
	}, nil
}
