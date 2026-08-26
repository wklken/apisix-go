package compiler

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
)

type blockingHTTPClusterObserver struct {
	proxy.NopClusterObserver
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (observer *blockingHTTPClusterObserver) SetHealth(string, string, bool) {
	observer.once.Do(func() { close(observer.entered) })
	<-observer.release
}

func TestHTTPClusterFinalReleaseRetriesAfterHealthResidual(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	observer := &blockingHTTPClusterObserver{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	clusterObservers, err := newClusterObserverRegistry(observer)
	if err != nil {
		t.Fatal(err)
	}
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	t.Cleanup(func() { releaseOnce.Do(func() { close(observer.release) }) })
	prepared.clusterObservers = clusterObservers
	config := proxy.ClusterConfig{
		Name:    "retryable-health",
		Targets: map[string]int{upstream.URL: 1},
		Checks: map[string]any{"active": map[string]any{
			"type": "http", "http_path": "/", "timeout": 1,
			"healthy": map[string]any{"interval": 1, "successes": 1},
			"unhealthy": map[string]any{
				"interval": 1, "http_failures": 1, "tcp_failures": 1, "timeouts": 1,
				"http_statuses": []any{http.StatusInternalServerError},
			},
		}},
		MaxInFlight: proxy.DefaultMaxInFlight,
	}
	cluster, err := prepared.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("active health transition did not reach observer")
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := prepared.Close(short)
	var residual *runtime.TaskResidualError
	digest, digestErr := config.Key()
	if digestErr != nil {
		t.Fatal(digestErr)
	}
	want := []runtime.TaskResidual{{
		Owner: "core/proxy-cluster/" + hex.EncodeToString(digest[:]) + "/active-health",
	}}
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) ||
		!slices.Equal(residual.Residuals(), want) {
		t.Fatalf("first Close() = %v, residuals = %v, want %v", first, residual, want)
	}
	if cluster.Closed() {
		t.Fatal("incomplete close released cluster transports")
	}
	clusterObservers.mu.Lock()
	retainedNames, retainedTargets := len(clusterObservers.names), len(clusterObservers.targets)
	clusterObservers.mu.Unlock()
	if retainedNames != 1 || retainedTargets != 1 {
		t.Fatalf("incomplete close released observer refs names=%d targets=%d", retainedNames, retainedTargets)
	}

	releaseOnce.Do(func() { close(observer.release) })
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if !cluster.Closed() || fixture.registry.Len() != 0 {
		t.Fatalf("terminal close cluster/registry = %t/%d", cluster.Closed(), fixture.registry.Len())
	}
}

type blockingHTTPClusterTaskOwner struct {
	owner   *runtime.TaskOwner
	entered chan struct{}
	release <-chan struct{}
}

func (owner *blockingHTTPClusterTaskOwner) Go(
	component string,
	run func(context.Context) error,
) error {
	return owner.owner.Go(component, func(ctx context.Context) error {
		owner.entered <- struct{}{}
		<-owner.release
		return run(ctx)
	})
}

func TestHTTPClusterFinalReleaseDeduplicatesMultipleActiveHealthResiduals(t *testing.T) {
	config := proxy.ClusterConfig{
		Name: "deduplicated-health",
		Targets: map[string]int{
			"http://127.0.0.1:9080": 1,
			"http://127.0.0.1:9081": 1,
		},
		Checks: map[string]any{"active": map[string]any{
			"type": "http", "http_path": "/", "timeout": 1,
			"healthy": map[string]any{"interval": 1, "successes": 1},
			"unhealthy": map[string]any{
				"interval": 1, "http_failures": 1, "tcp_failures": 1, "timeouts": 1,
			},
		}},
		MaxInFlight: proxy.DefaultMaxInFlight,
	}
	digest, err := config.Key()
	if err != nil {
		t.Fatal(err)
	}
	wantOwner := "core/proxy-cluster/" + hex.EncodeToString(digest[:]) + "/active-health"
	registry := runtime.NewResourceRegistry()
	entered := make(chan struct{}, len(config.Targets))
	release := make(chan struct{})
	var releaseOnce sync.Once
	var observerReleases atomic.Int32
	lease, err := runtime.Acquire(
		context.Background(),
		registry,
		runtime.ResourceKey{Kind: "proxy-cluster", Scope: "http-cluster/v1", Digest: digest},
		func(context.Context) (*proxy.Cluster, func(context.Context) error, error) {
			tasks := runtime.NewTaskRegistry(context.Background(), nil)
			owner, ownerErr := runtime.NewTaskOwner(
				tasks,
				"core/proxy-cluster/"+hex.EncodeToString(digest[:]),
				runtime.TaskCore,
			)
			if ownerErr != nil {
				return nil, nil, ownerErr
			}
			blockingOwner := &blockingHTTPClusterTaskOwner{
				owner: owner, entered: entered, release: release,
			}
			stopTasks := func(ctx context.Context) error {
				_, stopErr := tasks.Stop(ctx)
				return stopErr
			}
			cluster, createErr := proxy.NewOwnedCluster(
				config,
				proxy.NopClusterObserver{},
				blockingOwner,
				stopTasks,
			)
			if createErr != nil {
				_, _ = tasks.Stop(context.Background())
				return nil, nil, createErr
			}
			return cluster, func(closeCtx context.Context) error {
				if closeErr := cluster.CloseContext(closeCtx); closeErr != nil {
					return closeErr
				}
				observerReleases.Add(1)
				return nil
			}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = lease.Release(context.Background())
	})
	for index := range len(config.Targets) {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatalf("active-health task %d did not enter owner callback", index+1)
		}
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := lease.Release(short)
	var residual *runtime.TaskResidualError
	want := []runtime.TaskResidual{{Owner: wantOwner}}
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) ||
		!slices.Equal(residual.Residuals(), want) {
		t.Fatalf("first Release() = %v, residuals = %v, want %v", first, residual, want)
	}
	cluster := lease.Value()
	if cluster.Closed() || observerReleases.Load() != 0 {
		t.Fatalf(
			"incomplete release closed cluster=%t observer releases=%d",
			cluster.Closed(),
			observerReleases.Load(),
		)
	}

	releaseOnce.Do(func() { close(release) })
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("retry Release() error = %v", err)
	}
	if !cluster.Closed() || observerReleases.Load() != 1 || registry.Len() != 0 {
		t.Fatalf(
			"terminal release cluster=%t observer=%d registry=%d",
			cluster.Closed(),
			observerReleases.Load(),
			registry.Len(),
		)
	}
}

type httpClusterPrepareContextKey struct{}

func TestHTTPClusterActiveHealthDoesNotRetainPrepareContext(t *testing.T) {
	probes := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	failures := make(chan runtime.TaskFailure, 1)
	prepared.taskFailure = func(failure runtime.TaskFailure) { failures <- failure }
	traceCalls := make(chan struct{}, 1)
	trace := &httptrace.ClientTrace{GetConn: func(string) {
		select {
		case traceCalls <- struct{}{}:
		default:
		}
	}}
	prepareCtx := context.WithValue(context.Background(), httpClusterPrepareContextKey{}, "prepare-sentinel")
	prepareCtx = httptrace.WithClientTrace(prepareCtx, trace)
	cluster, err := prepared.acquireHTTPCluster(prepareCtx, proxy.ClusterConfig{
		Name:    "detached-context",
		Targets: map[string]int{upstream.URL: 1},
		Checks: map[string]any{"active": map[string]any{
			"type": "http", "http_path": "/", "timeout": 1,
			"healthy": map[string]any{"interval": 1, "successes": 1},
			"unhealthy": map[string]any{
				"interval": 1, "http_failures": 1, "tcp_failures": 1, "timeouts": 1,
			},
		}},
		MaxInFlight: proxy.DefaultMaxInFlight,
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitHTTPClusterProbe(t, probes)
	select {
	case <-traceCalls:
		t.Fatal("active health probe retained the Prepare context client trace")
	default:
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !cluster.Closed() || fixture.registry.Len() != 0 {
		t.Fatalf("terminal close cluster/registry = %t/%d", cluster.Closed(), fixture.registry.Len())
	}
	select {
	case failure := <-failures:
		t.Fatalf("unexpected resource task failure = %#v", failure)
	default:
	}
}

func TestHTTPClusterHealthOutlivesCreatingGenerationWhileReused(t *testing.T) {
	probes := make(chan struct{}, 8)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case probes <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

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
	config := proxy.ClusterConfig{
		Name:    "shared-health",
		Targets: map[string]int{upstream.URL: 1},
		Checks: map[string]any{"active": map[string]any{
			"type": "http", "http_path": "/", "timeout": 1,
			"healthy": map[string]any{"interval": 1, "successes": 1},
			"unhealthy": map[string]any{
				"interval": 1, "http_failures": 1, "tcp_failures": 1, "timeouts": 1,
			},
		}},
		MaxInFlight: proxy.DefaultMaxInFlight,
	}
	oldCluster, err := old.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	nextCluster, err := next.acquireHTTPCluster(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if oldCluster != nextCluster {
		t.Fatal("equal active-health cluster config was not reused")
	}
	awaitHTTPClusterProbe(t, probes)
	if err := old.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if oldCluster.Closed() {
		t.Fatal("creating generation retirement closed a reused cluster")
	}
	awaitHTTPClusterProbe(t, probes)
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !nextCluster.Closed() || sharedResources.Len() != 0 {
		t.Fatalf("final retirement cluster closed/registry = %t/%d", nextCluster.Closed(), sharedResources.Len())
	}
}

func awaitHTTPClusterProbe(t *testing.T, probes <-chan struct{}) {
	t.Helper()
	select {
	case <-probes:
	case <-time.After(2 * time.Second):
		t.Fatal("active health probe did not run")
	}
}

type cleanupTraceClusterObserver struct {
	proxy.NopClusterObserver
	calls *[]string
}

func (observer cleanupTraceClusterObserver) DeleteCluster(string) {
	*observer.calls = append(*observer.calls, "finalize-cluster")
}

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

func TestHTTPClusterLeaseUsesRetryableResourceFinalizationPhase(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, nil, nil)
	var calls []string
	clusterObservers, err := newClusterObserverRegistry(cleanupTraceClusterObserver{calls: &calls})
	if err != nil {
		t.Fatal(err)
	}
	prepared.clusterObservers = clusterObservers
	cluster, err := prepared.acquireHTTPCluster(context.Background(), proxy.ClusterConfig{
		Name: "retryable-finalizer",
		Targets: map[string]int{
			"http://127.0.0.1:9080": 1,
		},
		MaxInFlight: proxy.DefaultMaxInFlight,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.cleanup.Own(cleanupRelease, "sentinel", func(context.Context) error {
		calls = append(calls, "release-sentinel")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	quiesceAttempts := 0
	transient := errors.New("tasks still running")
	if err := prepared.cleanup.Own(cleanupQuiesce, "retryable-tasks", func(context.Context) error {
		quiesceAttempts++
		if quiesceAttempts == 1 {
			return transient
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := prepared.cleanup.Close(context.Background()); !errors.Is(err, transient) {
		t.Fatalf("first Close error = %v, want %v", err, transient)
	}
	if cluster.Closed() || fixture.registry.Len() != 1 || len(calls) != 0 {
		t.Fatalf(
			"first attempt cluster closed/registry/calls = %v/%d/%v",
			cluster.Closed(),
			fixture.registry.Len(),
			calls,
		)
	}
	if err := prepared.cleanup.Close(context.Background()); err != nil {
		t.Fatalf("retry Close error = %v", err)
	}
	want := []string{"finalize-cluster", "release-sentinel"}
	if !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if !cluster.Closed() || fixture.registry.Len() != 0 {
		t.Fatalf("terminal cluster closed/registry = %v/%d", cluster.Closed(), fixture.registry.Len())
	}
}
