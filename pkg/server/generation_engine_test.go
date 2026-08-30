package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

type generationEngineFixture struct {
	engine   *GenerationEngine
	factory  *compiler.WorkerCompilerFactory
	resolver *secret.GenerationSecretResolver
}

func newGenerationEngineFixture(t *testing.T) *generationEngineFixture {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	encryption := data_encryption.NewService(false, nil, catalog)
	resolver, err := secret.NewGenerationSecretResolver(encryption)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest,
		&config.EffectiveConfig{},
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{
			Cluster: proxy.NopClusterObserver{}, Stream: func(streamruntime.Result) {},
		},
	)
	if err != nil {
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}
	engine, err := NewGenerationEngine(&Server{}, factory)
	if err != nil {
		_ = factory.Close(context.Background())
		_ = resolver.Close(context.Background())
		t.Fatal(err)
	}
	fixture := &generationEngineFixture{engine: engine, factory: factory, resolver: resolver}
	t.Cleanup(func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("GenerationEngine.Close() error = %v", err)
		}
		if err := resolver.Close(context.Background()); err != nil {
			t.Errorf("GenerationSecretResolver.Close() error = %v", err)
		}
	})
	return fixture
}

func generationEngineInput(
	t *testing.T,
	revision uint64,
	domains ...generation.Domain,
) (generation.ApplyTicket, generation.Snapshot) {
	t.Helper()
	resources := make([]generation.Resource, 0, 1)
	if slices.Contains(domains, generation.DomainStream) {
		resources = append(resources, generation.Resource{
			Key: generation.ResourceKey{Kind: "stream_routes", ID: "tcp"},
			Value: []byte(`{
				"id":"tcp",
				"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
			}`),
		})
	}
	desired, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	return generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   desired.Digest(),
		Cursor: generation.ProviderCursor{
			Provider: "generation-engine-test", Revision: strconv.FormatUint(revision, 10),
		},
		RequiredDomains: slices.Clone(domains),
	}, desired
}

func prepareEngineGeneration(
	t *testing.T,
	engine *GenerationEngine,
	revision uint64,
	domains ...generation.Domain,
) generation.PublicationSet {
	t.Helper()
	ticket, desired := generationEngineInput(t, revision, domains...)
	set, err := engine.Publish(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return set
}

func prepareEngineStreamGeneration(
	t *testing.T,
	engine *GenerationEngine,
	revision uint64,
	routeIDs ...string,
) generation.PublicationSet {
	t.Helper()
	resources := make([]generation.Resource, 0, len(routeIDs))
	for index, routeID := range routeIDs {
		resources = append(resources, generation.Resource{
			Key: generation.ResourceKey{Kind: "stream_routes", ID: routeID},
			Value: fmt.Appendf(nil,
				`{"id":%q,"remote_addr":%q,"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}}`,
				routeID,
				fmt.Sprintf("192.0.2.%d", index+1),
			),
		})
	}
	desired, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   desired.Digest(),
		Cursor: generation.ProviderCursor{
			Provider: "generation-engine-stream-metrics-test",
			Revision: strconv.FormatUint(revision, 10),
		},
		RequiredDomains: []generation.Domain{generation.DomainStream},
	}
	set, err := engine.Publish(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return set
}

func installGenerationStreamMetrics(t *testing.T) {
	t.Helper()
	old := metrics.StreamConnections
	metrics.SetStreamRoutes(nil)
	metrics.StreamConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_generation_stream_connections"},
		[]string{"route"},
	)
	t.Cleanup(func() {
		metrics.SetStreamRoutes(nil)
		metrics.StreamConnections = old
	})
}

func assertGenerationStreamMetricDelta(t *testing.T, routeID string, want float64) {
	t.Helper()
	counter := metrics.StreamConnections.WithLabelValues(routeID)
	before := &dto.Metric{}
	if err := counter.Write(before); err != nil {
		t.Fatal(err)
	}
	metrics.RecordStreamConnection(routeID)
	after := &dto.Metric{}
	if err := counter.Write(after); err != nil {
		t.Fatal(err)
	}
	if got := after.GetCounter().GetValue() - before.GetCounter().GetValue(); got != want {
		t.Fatalf("stream metric delta for route %q = %v, want %v", routeID, got, want)
	}
}

func TestGenerationEnginePublicSurfaceIsPublishAndClose(t *testing.T) {
	typeOf := reflect.TypeFor[*GenerationEngine]()
	want := map[string]struct{}{"Publish": {}, "Close": {}}
	for method := range typeOf.Methods() {
		delete(want, method.Name)
	}
	if len(want) != 0 || typeOf.NumMethod() != 2 {
		t.Fatalf("GenerationEngine public methods = %d, missing = %v", typeOf.NumMethod(), want)
	}
}

func TestGenerationEnginePublishesCompositeBundleAtomically(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 1, generation.DomainHTTP, generation.DomainStream)
	active := fixture.engine.active.Load()
	if active == nil || active.http == nil || active.http != active.stream {
		t.Fatalf("composite active bundle = %#v", active)
	}
	if got := active.http.activeDomains; got != ownerDomainAll {
		t.Fatalf("composite owner domains = %d", got)
	}
}

func TestGenerationEngineDomainPublishPreservesUntouchedOwner(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 2, generation.DomainHTTP, generation.DomainStream)
	previous := fixture.engine.active.Load()
	prepareEngineGeneration(t, fixture.engine, 3, generation.DomainHTTP)
	active := fixture.engine.active.Load()
	if active.http == previous.http || active.stream != previous.stream {
		t.Fatal("HTTP publication did not preserve the exact stream owner")
	}
}

func TestGenerationEngineFailedPublishLeavesActiveBundleUnchanged(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 4, generation.DomainHTTP)
	previous := fixture.engine.active.Load()
	wantErr := errors.New("publication checkpoint failed")
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "candidate-bundle-published" {
			return wantErr
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	ticket, desired := generationEngineInput(t, 5, generation.DomainHTTP)
	if _, err := fixture.engine.Publish(context.Background(), ticket, desired, nil); !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v", err)
	}
	if fixture.engine.active.Load() != previous {
		t.Fatal("failed publication changed the active bundle")
	}
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = nil
	fixture.engine.mu.Unlock()
}

func TestGenerationEngineRetiresPredecessorAfterLeaseDrain(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 6, generation.DomainHTTP)
	predecessor := fixture.engine.active.Load().http
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("predecessor HTTP lease unavailable")
	}
	prepareEngineGeneration(t, fixture.engine, 7, generation.DomainHTTP)
	select {
	case <-predecessor.closeDone:
		t.Fatal("predecessor closed while its lease remained live")
	default:
	}
	lease.Release()
	select {
	case <-predecessor.closeDone:
	case <-time.After(time.Second):
		t.Fatal("predecessor did not retire after its lease drained")
	}
}

func TestGenerationEngineStreamMetricsRetainDrainingRoutes(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	prepareEngineStreamGeneration(t, fixture.engine, 8, "old")
	old := fixture.engine.active.Load().stream
	lease, ok := fixture.engine.acquireStream()
	if !ok {
		t.Fatal("old stream lease unavailable")
	}
	prepareEngineStreamGeneration(t, fixture.engine, 9, "new")
	assertGenerationStreamMetricDelta(t, "old", 1)
	assertGenerationStreamMetricDelta(t, "new", 1)
	lease.Release()
	select {
	case <-old.closeDone:
	case <-time.After(time.Second):
		t.Fatal("old stream generation did not retire")
	}
	assertGenerationStreamMetricDelta(t, "old", 0)
	assertGenerationStreamMetricDelta(t, "new", 1)
}

func TestGenerationEngineCloseWaitsForLeasesAndRejectsPublish(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 10, generation.DomainHTTP)
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("HTTP lease unavailable")
	}
	done := make(chan error, 1)
	go func() { done <- fixture.engine.Close(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Close() returned before lease drain: %v", err)
	default:
	}
	lease.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after lease drain")
	}
	ticket, desired := generationEngineInput(t, 11, generation.DomainHTTP)
	if _, err := fixture.engine.Publish(
		context.Background(),
		ticket,
		desired,
		nil,
	); !errors.Is(
		err,
		errGenerationEngineClosed,
	) {
		t.Fatalf("Publish() after Close error = %v", err)
	}
}

func TestGenerationEngineConcurrentCloseReplaysResult(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	prepareEngineGeneration(t, fixture.engine, 12, generation.DomainHTTP)
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("HTTP lease unavailable")
	}
	errorsSeen := make(chan error, 2)
	var started sync.WaitGroup
	started.Add(2)
	for range 2 {
		go func() {
			started.Done()
			errorsSeen <- fixture.engine.Close(context.Background())
		}()
	}
	started.Wait()
	lease.Release()
	first, second := <-errorsSeen, <-errorsSeen
	if first != second {
		t.Fatalf("Close() results differ: %v / %v", first, second)
	}
}
