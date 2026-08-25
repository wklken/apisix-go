package compiler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

type preparedTrafficSplitRuntimeAcquirer struct {
	prepared *PreparedGeneration
	ctx      context.Context
	route    resource.Route
	ssls     map[string]resource.SSL
}

func (acquirer *preparedTrafficSplitRuntimeAcquirer) Acquire(
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
) (*traffic_split.Runtime, error) {
	if acquirer == nil || acquirer.prepared == nil || acquirer.prepared.effective == nil {
		return nil, fmt.Errorf("%w: traffic-split runtime owner is incomplete", ErrInvalidInput)
	}
	config, err := routepkg.PlanTrafficSplitCluster(
		acquirer.route,
		upstream,
		targets,
		priorities,
		acquirer.ssls,
		&acquirer.prepared.effective.Config,
	)
	if err != nil {
		return nil, err
	}
	cluster, err := acquirer.prepared.acquireHTTPCluster(acquirer.ctx, config)
	if err != nil {
		return nil, err
	}
	return &traffic_split.Runtime{
		LoadBalancer: cluster.LoadBalancer(), RoundTripper: cluster.RoundTripper(),
	}, nil
}

func (prepared *PreparedGeneration) httpRuntimeContextForRoute(
	ctx context.Context,
	routeResource resource.Route,
	plan *httpPreparationPlan,
) (effectiveBindingRuntimeContext, error) {
	if prepared == nil || ctx == nil || prepared.effective == nil || plan == nil || plan.publicAPIRegistry == nil ||
		routeResource.ID == "" {
		return effectiveBindingRuntimeContext{}, fmt.Errorf(
			"%w: HTTP route runtime context is incomplete",
			ErrInvalidInput,
		)
	}
	ownedRoute, err := cloneEffectiveRoute(routeResource)
	if err != nil {
		return effectiveBindingRuntimeContext{}, fmt.Errorf(
			"%w: HTTP route runtime context is invalid",
			ErrInvalidInput,
		)
	}
	resolver := func(id string) (resource.Upstream, error) {
		upstream, exists := plan.resources.upstreams[id]
		if !exists {
			return resource.Upstream{}, fmt.Errorf("upstream %q is missing", id)
		}
		cloned, cloneErr := cloneEffectiveUpstream(upstream)
		if cloneErr != nil {
			return resource.Upstream{}, fmt.Errorf("upstream %q is not ownable", id)
		}
		return cloned, nil
	}
	return effectiveBindingRuntimeContext{
		configured: true, enabledFactories: slices.Clone(plan.enabledFactories),
		publicAPIRegistry: plan.publicAPIRegistry,
		serverAddr:        httpPreparationServerAddr(prepared),
		proxyCacheZones:   slices.Clone(prepared.effective.Config.Apisix.ProxyCache.Zones),
		runtimeAcquirer: &preparedTrafficSplitRuntimeAcquirer{
			prepared: prepared, ctx: ctx, route: ownedRoute, ssls: plan.resources.ssls,
		},
		upstreamResolver: resolver,
	}, nil
}

func httpPreparationServerAddr(prepared *PreparedGeneration) string {
	addresses := prepared.effective.Config.Apisix.ListenAddresses()
	if len(addresses) == 0 {
		return ""
	}
	if strings.HasPrefix(addresses[0], ":") {
		return "0.0.0.0" + addresses[0]
	}
	return addresses[0]
}
