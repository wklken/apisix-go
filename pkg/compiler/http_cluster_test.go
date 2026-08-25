package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestAcquireHTTPClusterSharesCanonicalConfigAndGenerationOwnsEveryLease(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	config := proxy.ClusterConfig{
		Name: "shared", Targets: map[string]int{"http://127.0.0.1:9080": 1},
		MaxInFlight: proxy.DefaultMaxInFlight,
	}
	first, err := prepared.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepared.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || first != second || fixture.registry.Len() != 1 {
		t.Fatalf("cluster reuse = %p/%p registry=%d", first, second, fixture.registry.Len())
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !first.Closed() || fixture.registry.Len() != 0 {
		t.Fatalf("cluster closed/registry = %v/%d", first.Closed(), fixture.registry.Len())
	}
}

func TestAcquireHTTPClusterRollbackReleasesTentativeLease(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	checkpoint, err := prepared.cleanup.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := prepared.acquireHTTPCluster(context.Background(), proxy.ClusterConfig{
		Name: "tentative", Targets: map[string]int{"http://127.0.0.1:9081": 1},
		MaxInFlight: proxy.DefaultMaxInFlight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.cleanup.Rollback(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	if !cluster.Closed() || fixture.registry.Len() != 0 {
		t.Fatalf("rolled back cluster closed/registry = %v/%d", cluster.Closed(), fixture.registry.Len())
	}
	if prepared.terminal {
		t.Fatal("cluster rollback terminally closed generation")
	}
}

func TestAcquireHTTPClusterObserverLifetimeAcrossGenerationOverlap(t *testing.T) {
	observer := &recordingRuntimeObserver{}
	clusterObservers, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	sharedResources := runtime.NewResourceRegistry()
	old, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	next, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	old.registry, next.registry = sharedResources, sharedResources
	old.clusterObservers, next.clusterObservers = clusterObservers, clusterObservers

	oldCluster, err := old.acquireHTTPCluster(context.Background(), proxy.ClusterConfig{
		Name: "orders", Targets: map[string]int{"http://127.0.0.1:9080": 1},
		MaxInFlight: proxy.DefaultMaxInFlight,
	})
	if err != nil {
		t.Fatal(err)
	}
	nextCluster, err := next.acquireHTTPCluster(context.Background(), proxy.ClusterConfig{
		Name: "orders", Targets: map[string]int{"http://127.0.0.1:9081": 1},
		MaxInFlight: proxy.DefaultMaxInFlight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if oldCluster == nextCluster {
		t.Fatal("different effective cluster configurations were reused")
	}
	if err := old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	clusters, targets, _ := observer.snapshot()
	if len(clusters) != 0 || len(targets) != 1 || targets[0].target != "http://127.0.0.1:9080" {
		t.Fatalf("old retirement deleted replacement metrics: clusters=%v targets=%v", clusters, targets)
	}
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	clusters, targets, _ = observer.snapshot()
	if len(clusters) != 1 || clusters[0] != "orders" || len(targets) != 2 ||
		targets[1].target != "http://127.0.0.1:9081" {
		t.Fatalf("final retirement metrics = clusters=%v targets=%v", clusters, targets)
	}
}

func TestAcquireHTTPClusterExactConfigSharesObserverLifetime(t *testing.T) {
	observer := &recordingRuntimeObserver{}
	clusterObservers, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	sharedResources := runtime.NewResourceRegistry()
	first, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	second, _ := newEffectiveBindingMaterializerFixture(t, nil, nil)
	first.registry, second.registry = sharedResources, sharedResources
	first.clusterObservers, second.clusterObservers = clusterObservers, clusterObservers
	config := proxy.ClusterConfig{
		Name: "shared", Targets: map[string]int{"http://127.0.0.1:9080": 1},
		MaxInFlight: proxy.DefaultMaxInFlight,
	}
	firstCluster, err := first.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	secondCluster, err := second.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if firstCluster != secondCluster {
		t.Fatal("exact cluster configuration was not reused")
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	clusters, targets, initialized := observer.snapshot()
	if len(clusters) != 0 || len(targets) != 0 || len(initialized) != 1 {
		t.Fatalf(
			"first exact-config retirement = clusters=%v targets=%v initialized=%v",
			clusters,
			targets,
			initialized,
		)
	}
	if err := second.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	clusters, targets, _ = observer.snapshot()
	if len(clusters) != 1 || len(targets) != 1 {
		t.Fatalf("final exact-config retirement = clusters=%v targets=%v", clusters, targets)
	}
}
