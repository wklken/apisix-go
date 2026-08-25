package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/proxy"
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
