package compiler

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
)

type httpClusterLeaseSlot struct {
	mu    sync.Mutex
	lease *runtime.ResourceLease[*proxy.Cluster]
}

func (slot *httpClusterLeaseSlot) adopt(lease *runtime.ResourceLease[*proxy.Cluster]) {
	slot.mu.Lock()
	slot.lease = lease
	slot.mu.Unlock()
}

func (slot *httpClusterLeaseSlot) release(ctx context.Context) error {
	slot.mu.Lock()
	lease := slot.lease
	slot.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.Release(ctx)
}

func (prepared *PreparedGeneration) acquireHTTPCluster(
	ctx context.Context,
	config proxy.ClusterConfig,
) (*proxy.Cluster, error) {
	if prepared == nil || ctx == nil || prepared.registry == nil || prepared.cleanup == nil {
		return nil, fmt.Errorf("%w: HTTP cluster owner is incomplete", ErrInvalidInput)
	}
	owned, err := cloneHTTPClusterConfig(config)
	if err != nil {
		return nil, err
	}
	digest, err := owned.Key()
	if err != nil {
		return nil, fmt.Errorf("%w: HTTP cluster identity is invalid", ErrInvalidInput)
	}
	slot := &httpClusterLeaseSlot{}
	if err := prepared.cleanup.Own(cleanupRelease, "http-cluster/"+owned.Name, slot.release); err != nil {
		return nil, err
	}
	lease, err := runtime.Acquire(
		context.WithoutCancel(ctx),
		prepared.registry,
		runtime.ResourceKey{Kind: "proxy-cluster", Scope: "http-cluster/v1", Digest: digest},
		func(context.Context) (*proxy.Cluster, func(context.Context) error, error) {
			cluster, createErr := proxy.NewCluster(owned, proxy.NopClusterObserver{})
			if createErr != nil {
				return nil, nil, createErr
			}
			return cluster, func(context.Context) error {
				cluster.Close()
				return nil
			}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	slot.adopt(lease)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return lease.Value(), nil
}

func cloneHTTPClusterConfig(source proxy.ClusterConfig) (proxy.ClusterConfig, error) {
	cloned := source
	cloned.Targets = maps.Clone(source.Targets)
	cloned.Priorities = maps.Clone(source.Priorities)
	if source.Checks != nil {
		owned, err := cloneEffectiveBindingValue(source.Checks)
		if err != nil {
			return proxy.ClusterConfig{}, fmt.Errorf("%w: HTTP cluster checks are not ownable", ErrInvalidInput)
		}
		cloned.Checks = owned.(map[string]any)
	}
	return cloned, nil
}
