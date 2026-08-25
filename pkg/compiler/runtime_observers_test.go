package compiler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/proxy"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

type recordingRuntimeObserver struct {
	mu          sync.Mutex
	clusters    []string
	targets     []clusterObserverTarget
	initialized []clusterObserverTarget
	inFlight    int
	retries     int
	health      int
	rejected    int
}

type clusterObserverTarget struct {
	cluster string
	target  string
}

type blockingDeleteRuntimeObserver struct {
	*recordingRuntimeObserver
	deleteStarted chan struct{}
	allowDelete   chan struct{}
	blockOnce     sync.Once
}

func (observer *blockingDeleteRuntimeObserver) DeleteCluster(cluster string) {
	observer.blockOnce.Do(func() {
		close(observer.deleteStarted)
		<-observer.allowDelete
	})
	observer.recordingRuntimeObserver.DeleteCluster(cluster)
}

func workerTestRuntimeObservers() WorkerRuntimeObservers {
	return WorkerRuntimeObservers{
		Cluster: proxy.NopClusterObserver{},
		Stream:  func(streamruntime.Result) {},
	}
}

func (observer *recordingRuntimeObserver) SetInFlight(string, int) {
	observer.mu.Lock()
	observer.inFlight++
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) ObserveRetry(string, string) {
	observer.mu.Lock()
	observer.retries++
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) SetHealth(string, string, bool) {
	observer.mu.Lock()
	observer.health++
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) ObserveRejected(string) {
	observer.mu.Lock()
	observer.rejected++
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) DeleteCluster(cluster string) {
	observer.mu.Lock()
	observer.clusters = append(observer.clusters, cluster)
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) SetUpstreamStatus(cluster, target string, _ bool) {
	observer.mu.Lock()
	observer.initialized = append(observer.initialized, clusterObserverTarget{cluster: cluster, target: target})
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) DeleteUpstreamStatus(cluster, target string) {
	observer.mu.Lock()
	observer.targets = append(observer.targets, clusterObserverTarget{cluster: cluster, target: target})
	observer.mu.Unlock()
}

func (observer *recordingRuntimeObserver) snapshot() ([]string, []clusterObserverTarget, []clusterObserverTarget) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]string(nil), observer.clusters...),
		append([]clusterObserverTarget(nil), observer.targets...),
		append([]clusterObserverTarget(nil), observer.initialized...)
}

func TestWorkerCompilerFactoryRequiresRuntimeObservers(t *testing.T) {
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &workerTestMaterializer{digest: compiler.schemas.catalog.Digest()}
	if factory, err := NewWorkerCompilerFactory(
		manifest, workerTestEffective(manifest), materializer, WorkerRuntimeObservers{},
	); factory != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing observers = %#v/%v, want nil/ErrInvalidInput", factory, err)
	}

	var typedNil *recordingRuntimeObserver
	if factory, err := NewWorkerCompilerFactory(
		manifest,
		workerTestEffective(manifest),
		materializer,
		WorkerRuntimeObservers{Cluster: typedNil},
	); factory != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("typed-nil observer = %#v/%v, want nil/ErrInvalidInput", factory, err)
	}

	httpFactory, err := NewWorkerCompilerFactory(
		manifest,
		workerTestEffective(manifest),
		materializer,
		WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpFactory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	streamEffective := workerTestEffective(manifest)
	streamEffective.Config.Apisix.ProxyMode = "stream"
	streamEffective.Config.Apisix.StreamProxy.Tcp = []config.TcpListen{{Addr: "127.0.0.1:9100"}}
	if factory, err := NewWorkerCompilerFactory(
		manifest,
		streamEffective,
		materializer,
		WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}},
	); factory != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing stream observer = %#v/%v, want nil/ErrInvalidInput", factory, err)
	}
	streamFactory, err := NewWorkerCompilerFactory(
		manifest,
		streamEffective,
		materializer,
		WorkerRuntimeObservers{
			Cluster: proxy.NopClusterObserver{},
			Stream:  func(streamruntime.Result) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := streamFactory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClusterObserverDeletesMetricsOnlyAfterFinalSameNameLease(t *testing.T) {
	observer := &recordingRuntimeObserver{}
	registry, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	old := registry.acquire("orders", []string{"http://127.0.0.1:9080"})
	old.activate()
	next := registry.acquire("orders", []string{"http://127.0.0.1:9081"})
	next.activate()

	old.Release()
	clusters, targets, initialized := observer.snapshot()
	if len(clusters) != 0 {
		t.Fatalf("old release deleted live cluster metrics: %v", clusters)
	}
	if len(targets) != 1 || targets[0] != (clusterObserverTarget{
		cluster: "orders", target: "http://127.0.0.1:9080",
	}) {
		t.Fatalf("old release target deletes = %#v", targets)
	}
	if len(initialized) != 2 {
		t.Fatalf("initialized targets = %#v, want 2", initialized)
	}

	next.Release()
	clusters, targets, _ = observer.snapshot()
	if len(clusters) != 1 || clusters[0] != "orders" {
		t.Fatalf("final cluster deletes = %v", clusters)
	}
	if len(targets) != 2 || targets[1] != (clusterObserverTarget{
		cluster: "orders", target: "http://127.0.0.1:9081",
	}) {
		t.Fatalf("final target deletes = %#v", targets)
	}
}

func TestClusterObserverSharedTargetDeletesOnlyAfterFinalReference(t *testing.T) {
	observer := &recordingRuntimeObserver{}
	registry, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	first := registry.acquire("orders", []string{"http://127.0.0.1:9080"})
	first.activate()
	second := registry.acquire("orders", []string{"http://127.0.0.1:9080"})
	second.activate()
	first.Release()
	clusters, targets, initialized := observer.snapshot()
	if len(clusters) != 0 || len(targets) != 0 || len(initialized) != 1 {
		t.Fatalf("first shared release = clusters=%v targets=%v initialized=%v", clusters, targets, initialized)
	}
	second.Release()
	clusters, targets, _ = observer.snapshot()
	if len(clusters) != 1 || len(targets) != 1 {
		t.Fatalf("final shared release = clusters=%v targets=%v", clusters, targets)
	}
}

func TestClusterObserverSerializesFinalDeleteWithSameNameAcquire(t *testing.T) {
	observer := &blockingDeleteRuntimeObserver{
		recordingRuntimeObserver: &recordingRuntimeObserver{},
		deleteStarted:            make(chan struct{}),
		allowDelete:              make(chan struct{}),
	}
	registry, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	old := registry.acquire("orders", nil)
	old.activate()
	releaseDone := make(chan struct{})
	go func() {
		old.Release()
		close(releaseDone)
	}()

	select {
	case <-observer.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("old generation did not reach final metric deletion")
	}
	nextLease := make(chan *clusterObserverLease, 1)
	go func() {
		next := registry.acquire("orders", nil)
		next.activate()
		nextLease <- next
	}()

	var (
		next  *clusterObserverLease
		raced bool
	)
	select {
	case next = <-nextLease:
		raced = true
	case <-time.After(100 * time.Millisecond):
	}
	close(observer.allowDelete)
	select {
	case <-releaseDone:
	case <-time.After(time.Second):
		t.Fatal("old generation metric deletion did not complete")
	}
	if next == nil {
		select {
		case next = <-nextLease:
		case <-time.After(time.Second):
			t.Fatal("replacement acquire did not resume after deletion")
		}
	}
	next.Release()
	if raced {
		t.Fatal("same-name replacement acquired before old metric deletion completed")
	}
}
