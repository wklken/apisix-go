package compiler

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/proxy"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

// WorkerRuntimeObservers are fixed when a worker compiler factory is created
// and are inherited by every prepared generation it owns.
type WorkerRuntimeObservers struct {
	Cluster    proxy.ClusterObserver
	Stream     func(streamruntime.Result)
	ServerInfo *server_info.View
}

func validateWorkerRuntimeObservers(
	effective *config.EffectiveConfig,
	observers WorkerRuntimeObservers,
) error {
	if isNilInterface(observers.Cluster) {
		return fmt.Errorf("%w: cluster runtime observer is required", ErrInvalidInput)
	}
	if workerStreamProxyEnabled(effective) && observers.Stream == nil {
		return fmt.Errorf("%w: stream runtime observer is required", ErrInvalidInput)
	}
	return nil
}

func workerStreamProxyEnabled(effective *config.EffectiveConfig) bool {
	if effective == nil {
		return false
	}
	mode := strings.ToLower(strings.ReplaceAll(effective.Config.Apisix.ProxyMode, " ", ""))
	return mode == "stream" || mode == "http&stream" || mode == "stream&http"
}

type clusterObserverTargetKey struct {
	cluster string
	target  string
}

type clusterObserverRef struct {
	refs        int
	initialized bool
}

// clusterObserverRegistry keeps metric lifetimes correct while different
// immutable cluster resources with the same APISIX name overlap.
type clusterObserverRegistry struct {
	mu      sync.Mutex
	sink    proxy.ClusterObserver
	names   map[string]clusterObserverRef
	targets map[clusterObserverTargetKey]clusterObserverRef
}

func newClusterObserverRegistry(observer proxy.ClusterObserver) (*clusterObserverRegistry, error) {
	if isNilInterface(observer) {
		return nil, fmt.Errorf("%w: cluster runtime observer is required", ErrInvalidInput)
	}
	return &clusterObserverRegistry{
		sink: observer, names: make(map[string]clusterObserverRef),
		targets: make(map[clusterObserverTargetKey]clusterObserverRef),
	}, nil
}

func (registry *clusterObserverRegistry) acquire(name string, targets []string) *clusterObserverLease {
	ownedTargets := slices.Clone(targets)
	slices.Sort(ownedTargets)
	ownedTargets = slices.Compact(ownedTargets)
	registry.mu.Lock()
	nameRef := registry.names[name]
	nameRef.refs++
	registry.names[name] = nameRef
	for _, target := range ownedTargets {
		key := clusterObserverTargetKey{cluster: name, target: target}
		ref := registry.targets[key]
		ref.refs++
		registry.targets[key] = ref
	}
	registry.mu.Unlock()
	return &clusterObserverLease{
		registry: registry,
		name:     name,
		targets:  ownedTargets,
		observer: forwardingClusterObserver{sink: registry.sink},
	}
}

type clusterObserverLease struct {
	registry *clusterObserverRegistry
	name     string
	targets  []string
	observer proxy.ClusterObserver

	activateOnce sync.Once
	releaseOnce  sync.Once
}

func (lease *clusterObserverLease) Observer() proxy.ClusterObserver {
	if lease == nil {
		return proxy.NopClusterObserver{}
	}
	return lease.observer
}

func (lease *clusterObserverLease) activate() {
	if lease == nil || lease.registry == nil {
		return
	}
	lease.activateOnce.Do(func() {
		registry := lease.registry
		initialize := make([]clusterObserverTargetKey, 0, len(lease.targets))
		registry.mu.Lock()
		defer registry.mu.Unlock()
		nameRef := registry.names[lease.name]
		nameRef.initialized = true
		registry.names[lease.name] = nameRef
		for _, target := range lease.targets {
			key := clusterObserverTargetKey{cluster: lease.name, target: target}
			ref := registry.targets[key]
			if !ref.initialized {
				ref.initialized = true
				registry.targets[key] = ref
				initialize = append(initialize, key)
			}
		}
		if statusObserver, ok := registry.sink.(proxy.UpstreamStatusObserver); ok {
			for _, key := range initialize {
				statusObserver.SetUpstreamStatus(key.cluster, key.target, true)
			}
		}
	})
}

func (lease *clusterObserverLease) Release() {
	if lease == nil || lease.registry == nil {
		return
	}
	lease.releaseOnce.Do(func() {
		registry := lease.registry
		deleteTargets := make([]clusterObserverTargetKey, 0, len(lease.targets))
		deleteCluster := false
		registry.mu.Lock()
		defer registry.mu.Unlock()
		for _, target := range lease.targets {
			key := clusterObserverTargetKey{cluster: lease.name, target: target}
			ref := registry.targets[key]
			ref.refs--
			if ref.refs == 0 {
				delete(registry.targets, key)
				if ref.initialized {
					deleteTargets = append(deleteTargets, key)
				}
			} else {
				registry.targets[key] = ref
			}
		}
		nameRef := registry.names[lease.name]
		nameRef.refs--
		if nameRef.refs == 0 {
			delete(registry.names, lease.name)
			deleteCluster = nameRef.initialized
		} else {
			registry.names[lease.name] = nameRef
		}
		if statusObserver, ok := registry.sink.(proxy.UpstreamStatusObserver); ok {
			for _, key := range deleteTargets {
				statusObserver.DeleteUpstreamStatus(key.cluster, key.target)
			}
		}
		if deleteCluster {
			registry.sink.DeleteCluster(lease.name)
		}
	})
}

type forwardingClusterObserver struct {
	sink proxy.ClusterObserver
}

func (observer forwardingClusterObserver) SetInFlight(cluster string, delta int) {
	observer.sink.SetInFlight(cluster, delta)
}

func (observer forwardingClusterObserver) ObserveRetry(cluster, result string) {
	observer.sink.ObserveRetry(cluster, result)
}

func (observer forwardingClusterObserver) SetHealth(cluster, target string, healthy bool) {
	observer.sink.SetHealth(cluster, target, healthy)
}

func (observer forwardingClusterObserver) ObserveRejected(cluster string) {
	observer.sink.ObserveRejected(cluster)
}

// Resource cleanup owns delete signals through clusterObserverLease.Release.
func (forwardingClusterObserver) DeleteCluster(string) {}
