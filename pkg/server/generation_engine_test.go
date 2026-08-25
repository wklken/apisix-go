package server

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

type generationEngineFixture struct {
	engine   *GenerationEngine
	factory  *compiler.WorkerCompilerFactory
	resolver *secret.GenerationSecretResolver
}

var _ func(*Server, *compiler.WorkerCompilerFactory) (*GenerationEngine, error) = NewGenerationEngine

func newGenerationEngineFixture(t *testing.T) *generationEngineFixture {
	return newGenerationEngineFixtureWithRecovery(t, true)
}

func newUnrecoveredGenerationEngineFixture(t *testing.T) *generationEngineFixture {
	return newGenerationEngineFixtureWithRecovery(t, false)
}

func newGenerationEngineFixtureWithRecovery(t *testing.T, installRecovery bool) *generationEngineFixture {
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
	profiles := config.ProfileSelection{
		Compatibility: config.CompatibilityTarget(manifest.Target.Name),
		Security:      config.SecurityCompat,
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{
			CompatibilityTarget:  profiles.Compatibility,
			SecurityProfile:      profiles.Security,
			QualificationProfile: profiles.Qualification,
		},
		Profiles: profiles,
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest,
		effective,
		secret.NewMaterializer(encryption, resolver),
		compiler.WorkerRuntimeObservers{
			Cluster: proxy.NopClusterObserver{},
			Stream:  func(streamruntime.Result) {},
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
	if installRecovery {
		if err := engine.InstallRecovery(context.Background(), generation.RecoveryState{}); err != nil {
			_ = engine.Close(context.Background())
			_ = resolver.Close(context.Background())
			t.Fatal(err)
		}
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
			Provider: "generation-engine-test",
			Revision: string(rune('a' + revision%26)),
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
	set, err := engine.Prepare(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
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
	for _, routeID := range routeIDs {
		resources = append(resources, generation.Resource{
			Key:   generation.ResourceKey{Kind: "stream_routes", ID: routeID},
			Value: []byte(`{"id":"` + routeID + `","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}}`),
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
			Revision: string(rune('a' + revision%26)),
		},
		RequiredDomains: []generation.Domain{generation.DomainStream},
	}
	set, err := engine.Prepare(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
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

func activateEngineGeneration(
	t *testing.T,
	engine *GenerationEngine,
	token generation.PublicationToken,
	set generation.PublicationSet,
) {
	t.Helper()
	if err := engine.Activate(context.Background(), token, set); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
}

func TestGenerationEnginePrepareRetainsExactCompositeIdentity(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 10, generation.DomainHTTP, generation.DomainStream)
	if set.DesiredRevision != 10 || len(set.Domains) != 2 {
		t.Fatalf("Prepare() set = %#v", set)
	}
	record := fixture.engine.pending[mustEnginePreparedKey(t, set)]
	if record == nil || record.owner == nil || !reflect.DeepEqual(record.set, set) {
		t.Fatal("Prepare() did not retain the exact composite identity and owner")
	}
}

func TestGenerationEnginePrepareZeroDomainDoesNotCallFactory(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 11)
	if len(set.Domains) != 0 {
		t.Fatalf("zero-domain set = %#v", set)
	}
	record := fixture.engine.pending[mustEnginePreparedKey(t, set)]
	if record == nil || !record.synthetic || record.owner != nil {
		t.Fatalf("zero-domain pending record = %#v", record)
	}
}

func TestGenerationEngineDiscardRequiresExactIdentityAndClosesOnce(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	ticket, desired := generationEngineInput(t, 12, generation.DomainHTTP, generation.DomainStream)
	set, err := fixture.engine.Prepare(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := fixture.engine.pending[mustEnginePreparedKey(t, set)]
	if !reflect.DeepEqual(record.owner.prepared.PublicationSet(), record.set) {
		t.Fatalf(
			"stored identity differs from prepared identity: stored=%#v prepared=%#v",
			record.set,
			record.owner.prepared.PublicationSet(),
		)
	}
	mutations := map[string]func(*generation.PublicationSet){
		"desired revision":  func(value *generation.PublicationSet) { value.DesiredRevision++ },
		"domain membership": func(value *generation.PublicationSet) { delete(value.Domains, generation.DomainStream) },
		"artifact digest": func(value *generation.PublicationSet) {
			candidate := value.Domains[generation.DomainHTTP]
			candidate.Artifact.Digest[0]++
			value.Domains[generation.DomainHTTP] = candidate
		},
		"snapshot id": func(value *generation.PublicationSet) {
			candidate := value.Domains[generation.DomainHTTP]
			candidate.Artifact.Snapshot += "-forged"
			value.Domains[generation.DomainHTTP] = candidate
		},
		"decisions": func(value *generation.PublicationSet) {
			candidate := value.Domains[generation.DomainStream]
			candidate.Decisions[0].Code = "forged"
			value.Domains[generation.DomainStream] = candidate
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			wrong := cloneEnginePublicationSet(set)
			mutate(&wrong)
			if err := fixture.engine.DiscardPrepared(context.Background(), wrong); err == nil {
				t.Fatal("DiscardPrepared() accepted a mismatched publication identity")
			}
			if fixture.engine.pending[mustEnginePreparedKey(t, set)] != record ||
				record.owner.prepared.HTTP() == nil || record.owner.prepared.Stream() == nil {
				t.Fatal("mismatched discard touched the original pending owner")
			}
		})
	}
	if err := fixture.engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("DiscardPrepared() error = %v", err)
	}
	if record.owner.prepared.HTTP() != nil {
		t.Fatal("discard did not close the prepared generation")
	}
	if _, err := fixture.engine.Prepare(context.Background(), ticket, desired, nil); err != nil {
		t.Fatalf("Prepare() after exact discard did not observe detached factory owner: %v", err)
	}
}

func TestGenerationEngineDiscardConcurrentWaitersReplayThenForget(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	engine := fixture.engine
	set := prepareEngineGeneration(t, engine, 13, generation.DomainHTTP)
	record := engine.pending[mustEnginePreparedKey(t, set)]
	release := startGenerationEngineBlockingTask(t, record.owner, "discard-concurrent")
	defer release()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	second := make(chan error, 1)
	leaderStarted := make(chan struct{})
	waiterRegistered := make(chan struct{})
	var leaderOnce, registeredOnce sync.Once
	engine.mu.Lock()
	engine.checkpoint = func(stage string) error {
		switch stage {
		case "discard-attempt-started":
			leaderOnce.Do(func() { close(leaderStarted) })
		case "discard-waiter-registered":
			registeredOnce.Do(func() { close(waiterRegistered) })
		}
		return nil
	}
	engine.mu.Unlock()
	go func() { first <- engine.DiscardPrepared(firstCtx, set) }()
	select {
	case <-leaderStarted:
	case <-time.After(time.Second):
		t.Fatal("first discard did not become attempt leader")
	}
	go func() { second <- engine.DiscardPrepared(context.Background(), set) }()
	select {
	case <-waiterRegistered:
	case <-time.After(time.Second):
		t.Fatal("second discard did not register as a concurrent waiter")
	}
	cancelFirst()
	firstErr, secondErr := <-first, <-second
	var firstResidual, secondResidual *runtime.TaskResidualError
	if !errors.As(firstErr, &firstResidual) || !errors.As(secondErr, &secondResidual) ||
		!reflect.DeepEqual(firstResidual.Residuals(), secondResidual.Residuals()) {
		t.Fatalf("concurrent discard results = %v / %v", firstErr, secondErr)
	}
	engine.mu.Lock()
	retained := engine.pending[mustEnginePreparedKey(t, set)]
	engine.mu.Unlock()
	if retained != record || record.discard == nil || record.discard.terminal {
		t.Fatal("residual discard did not retain its exact pending record and incomplete attempt")
	}
	release()
	if err := engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("retry DiscardPrepared() error = %v", err)
	}
	if engine.pending[mustEnginePreparedKey(t, set)] != nil {
		t.Fatal("terminal discard retained pending record")
	}
}

func TestGenerationEngineDiscardResidualRetainsExactSetForRetry(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 14, generation.DomainHTTP)
	key := mustEnginePreparedKey(t, set)
	record := fixture.engine.pending[key]
	release := startGenerationEngineBlockingTask(t, record.owner, "discard-retry")
	defer release()

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := fixture.engine.DiscardPrepared(short, set)
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first DiscardPrepared() error = %v, residual = %#v", first, residual)
	}
	fixture.engine.mu.Lock()
	retained := fixture.engine.pending[key]
	attempt := record.discard
	fixture.engine.mu.Unlock()
	if retained != record || attempt == nil || attempt.terminal {
		t.Fatal("residual discard dropped the pending record or marked its attempt terminal")
	}
	activationErr := fixture.engine.Activate(context.Background(), "closing", set)
	if !errors.Is(activationErr, compiler.ErrPreparedSetMismatch) {
		t.Fatalf("Activate() during retained discard = %v", activationErr)
	}
	wrong := cloneEnginePublicationSet(set)
	wrongCandidate := wrong.Domains[generation.DomainHTTP]
	wrongCandidate.Artifact.Snapshot += "-wrong"
	wrong.Domains[generation.DomainHTTP] = wrongCandidate
	mismatchErr := fixture.engine.DiscardPrepared(context.Background(), wrong)
	if !errors.Is(mismatchErr, compiler.ErrPreparedSetMismatch) {
		t.Fatalf("mismatched DiscardPrepared() error = %v", mismatchErr)
	}
	if record.discard != attempt {
		t.Fatal("mismatched discard advanced the retained cleanup attempt")
	}

	release()
	if err := fixture.engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("retry DiscardPrepared() error = %v", err)
	}
	if fixture.engine.pending[key] != nil {
		t.Fatal("terminal retry retained pending record")
	}
}

func TestGenerationEngineDiscardOldWaiterReadsFirstAttemptWhenSecondFinishesFirst(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 15, generation.DomainHTTP)
	record := fixture.engine.pending[mustEnginePreparedKey(t, set)]
	release := startGenerationEngineBlockingTask(t, record.owner, "discard-old-waiter")
	defer release()

	leaderStarted := make(chan struct{})
	waiterRegistered := make(chan struct{})
	waiterObserved := make(chan struct{})
	allowWaiterReturn := make(chan struct{})
	var leaderOnce, registeredOnce, observedOnce sync.Once
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		switch stage {
		case "discard-attempt-started":
			leaderOnce.Do(func() { close(leaderStarted) })
		case "discard-waiter-registered":
			registeredOnce.Do(func() { close(waiterRegistered) })
		case "discard-waiter-observed":
			observedOnce.Do(func() {
				close(waiterObserved)
				<-allowWaiterReturn
			})
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	leader := make(chan error, 1)
	oldWaiter := make(chan error, 1)
	go func() { leader <- fixture.engine.DiscardPrepared(firstCtx, set) }()
	select {
	case <-leaderStarted:
	case <-time.After(time.Second):
		t.Fatal("attempt A leader did not start")
	}
	go func() { oldWaiter <- fixture.engine.DiscardPrepared(context.Background(), set) }()
	select {
	case <-waiterRegistered:
	case <-time.After(time.Second):
		t.Fatal("old waiter did not capture attempt A")
	}
	cancelFirst()
	firstErr := <-leader
	var firstResidual *runtime.TaskResidualError
	if !errors.As(firstErr, &firstResidual) {
		t.Fatalf("attempt A error = %v", firstErr)
	}
	select {
	case <-waiterObserved:
	case <-time.After(time.Second):
		t.Fatal("old waiter did not observe attempt A completion")
	}

	release()
	if err := fixture.engine.DiscardPrepared(context.Background(), set); err != nil {
		t.Fatalf("attempt B error = %v", err)
	}
	close(allowWaiterReturn)
	oldErr := <-oldWaiter
	var oldResidual *runtime.TaskResidualError
	if !errors.As(oldErr, &oldResidual) || !reflect.DeepEqual(oldResidual.Residuals(), firstResidual.Residuals()) {
		t.Fatalf("old waiter error = %v, want attempt A residual %v", oldErr, firstResidual.Residuals())
	}
}

func TestGenerationEnginePublicSurfaceIsFrozen(t *testing.T) {
	typeOf := reflect.TypeFor[*GenerationEngine]()
	want := map[string]struct{}{
		"Prepare": {}, "DiscardPrepared": {}, "Activate": {}, "RollbackActivation": {},
		"FinalizeActivation": {}, "ConfirmActive": {}, "InstallRecovery": {}, "Close": {},
	}
	for method := range typeOf.Methods() {
		delete(want, method.Name)
	}
	if len(want) != 0 || typeOf.NumMethod() != 8 {
		t.Fatalf("GenerationEngine public methods = %d, missing/extra = %v", typeOf.NumMethod(), want)
	}
	files, err := parser.ParseFile(token.NewFileSet(), "generation_engine.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range files.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "GenerationEngine" {
				continue
			}
			structure := typeSpec.Type.(*ast.StructType)
			for _, field := range structure.Fields.List {
				if len(field.Names) == 0 || ast.IsExported(field.Names[0].Name) {
					t.Fatalf("GenerationEngine exposes field or embedding at %v", field.Pos())
				}
			}
		}
	}
}

func TestGenerationEngineHTTPOnlyActivationPreservesStreamOwner(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	composite := prepareEngineGeneration(t, fixture.engine, 20, generation.DomainHTTP, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "composite", composite)
	fixture.engine.FinalizeActivation(context.Background(), "composite", composite)
	previous := fixture.engine.active.Load()
	next := prepareEngineGeneration(t, fixture.engine, 21, generation.DomainHTTP)
	activateEngineGeneration(t, fixture.engine, "http", next)
	active := fixture.engine.active.Load()
	if active.http == previous.http || active.stream != previous.stream {
		t.Fatal("HTTP-only activation did not preserve the exact stream owner")
	}
	if got := active.http.activeDomains; got != ownerDomainHTTP {
		t.Fatalf("new HTTP owner active domains = %d", got)
	}
}

func TestGenerationEngineStreamOnlyActivationPreservesHTTPOwner(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	composite := prepareEngineGeneration(t, fixture.engine, 22, generation.DomainHTTP, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "composite", composite)
	fixture.engine.FinalizeActivation(context.Background(), "composite", composite)
	previous := fixture.engine.active.Load()
	next := prepareEngineGeneration(t, fixture.engine, 23, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "stream", next)
	active := fixture.engine.active.Load()
	if active.stream == previous.stream || active.http != previous.http {
		t.Fatal("stream-only activation did not preserve the exact HTTP owner")
	}
	if got := active.stream.activeDomains; got != ownerDomainStream {
		t.Fatalf("new stream owner active domains = %d", got)
	}
}

func TestGenerationEngineCompositeActivationPublishesOneBundle(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 24, generation.DomainHTTP, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "composite", set)
	active := fixture.engine.active.Load()
	if active == nil || active.http == nil || active.http != active.stream {
		t.Fatalf("composite active bundle = %#v", active)
	}
	if got := active.http.activeDomains; got != ownerDomainAll {
		t.Fatalf("composite owner active domains = %d", got)
	}
}

func TestGenerationEngineConcurrentPrepareRejectsDuplicateIdentity(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	ticket, desired := generationEngineInput(t, 25, generation.DomainHTTP)
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(2)
	for range 2 {
		go func() {
			defer group.Done()
			<-start
			_, err := fixture.engine.Prepare(context.Background(), ticket, desired, nil)
			errorsSeen <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsSeen)
	var succeeded, failed int
	for err := range errorsSeen {
		if err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	if succeeded != 1 || failed != 1 || len(fixture.engine.pending) != 1 {
		t.Fatalf(
			"concurrent Prepare() succeeded=%d failed=%d pending=%d",
			succeeded,
			failed,
			len(fixture.engine.pending),
		)
	}
}

func TestGenerationEnginePrepareRejectsIdentityHeldByActivation(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	ticket, desired := generationEngineInput(t, 26)
	set, err := fixture.engine.Prepare(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	activateEngineGeneration(t, fixture.engine, "tentative", set)
	if _, err := fixture.engine.Prepare(context.Background(), ticket, desired, nil); err == nil {
		t.Fatal("Prepare() accepted an identity already held by a tentative activation")
	}
}

func cloneEnginePublicationSet(set generation.PublicationSet) generation.PublicationSet {
	cloned := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		candidate.Snapshot = candidate.Snapshot.Clone()
		candidate.Closure = slices.Clone(candidate.Closure)
		candidate.Decisions = slices.Clone(candidate.Decisions)
		cloned.Domains[domain] = candidate
	}
	return cloned
}

func mustEnginePreparedKey(t *testing.T, set generation.PublicationSet) preparedKey {
	t.Helper()
	key, err := preparedKeyFromSet(set)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func finalizeEngineGeneration(
	t *testing.T,
	engine *GenerationEngine,
	token generation.PublicationToken,
	set generation.PublicationSet,
) {
	t.Helper()
	activateEngineGeneration(t, engine, token, set)
	engine.FinalizeActivation(context.Background(), token, set)
}

func TestGenerationEngineRollbackRestoresCompletePredecessorBundle(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	oldSet := prepareEngineGeneration(t, fixture.engine, 30, generation.DomainHTTP, generation.DomainStream)
	finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
	previous := fixture.engine.active.Load()
	newSet := prepareEngineGeneration(t, fixture.engine, 31, generation.DomainHTTP, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "new", newSet)
	rejected := fixture.engine.active.Load().http
	if err := fixture.engine.RollbackActivation(context.Background(), "new", newSet); err != nil {
		t.Fatal(err)
	}
	if active := fixture.engine.active.Load(); active.http != previous.http || active.stream != previous.stream {
		t.Fatal("rollback did not restore the complete predecessor bundle")
	}
	select {
	case <-rejected.closeDone:
	default:
		t.Fatal("rollback returned before rejected owner closed")
	}
}

func TestGenerationEngineStreamMetricsRetainActiveAndDrainingRouteUnion(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	setA := prepareEngineStreamGeneration(t, fixture.engine, 80, "shared", "only-a")
	finalizeEngineGeneration(t, fixture.engine, "stream-a", setA)
	ownerA := fixture.engine.active.Load().stream
	leaseA, ok := fixture.engine.acquireStream()
	if !ok {
		t.Fatal("generation A stream lease unavailable")
	}
	t.Cleanup(leaseA.Release)

	setB := prepareEngineStreamGeneration(t, fixture.engine, 81, "shared", "only-b")
	finalizeEngineGeneration(t, fixture.engine, "stream-b", setB)
	assertGenerationStreamMetricDelta(t, "only-a", 1)
	assertGenerationStreamMetricDelta(t, "shared", 1)
	assertGenerationStreamMetricDelta(t, "only-b", 1)

	leaseA.Release()
	select {
	case <-ownerA.closeDone:
	case <-time.After(time.Second):
		t.Fatal("generation A did not retire after its stream lease drained")
	}
	assertGenerationStreamMetricDelta(t, "only-a", 0)
	assertGenerationStreamMetricDelta(t, "shared", 1)
	assertGenerationStreamMetricDelta(t, "only-b", 1)
}

func TestGenerationEngineStreamMetricsKeepRolledBackCandidateUntilLeaseDrains(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	setA := prepareEngineStreamGeneration(t, fixture.engine, 82, "active")
	finalizeEngineGeneration(t, fixture.engine, "stream-a", setA)
	setB := prepareEngineStreamGeneration(t, fixture.engine, 83, "rolled-back")
	activateEngineGeneration(t, fixture.engine, "stream-b", setB)
	leaseB, ok := fixture.engine.acquireStream()
	if !ok {
		t.Fatal("generation B stream lease unavailable")
	}
	t.Cleanup(leaseB.Release)

	retirementEntered := make(chan struct{})
	allowRetirement := make(chan struct{})
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			close(retirementEntered)
			<-allowRetirement
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		done <- fixture.engine.RollbackActivation(context.Background(), "stream-b", setB)
	}()
	select {
	case <-retirementEntered:
	case <-time.After(time.Second):
		leaseB.Release()
		t.Fatal("rolled-back generation did not enter retirement")
	}
	assertGenerationStreamMetricDelta(t, "active", 1)
	assertGenerationStreamMetricDelta(t, "rolled-back", 1)

	leaseB.Release()
	close(allowRetirement)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback did not finish after generation B drained")
	}
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = nil
	fixture.engine.mu.Unlock()
	assertGenerationStreamMetricDelta(t, "active", 1)
	assertGenerationStreamMetricDelta(t, "rolled-back", 0)
}

func TestGenerationEngineRollbackWaitsForTentativeLeaseButNotPredecessor(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	oldSet := prepareEngineGeneration(t, fixture.engine, 32, generation.DomainHTTP, generation.DomainStream)
	finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
	predecessorLease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("predecessor HTTP lease unavailable")
	}
	newSet := prepareEngineGeneration(t, fixture.engine, 33, generation.DomainHTTP, generation.DomainStream)
	activateEngineGeneration(t, fixture.engine, "new", newSet)
	candidateLease, ok := fixture.engine.acquireStream()
	if !ok {
		t.Fatal("candidate stream lease unavailable")
	}
	retirementEntered := make(chan struct{})
	allowRetirement := make(chan struct{})
	var retirementOnce sync.Once
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			retirementOnce.Do(func() {
				close(retirementEntered)
				<-allowRetirement
			})
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- fixture.engine.RollbackActivation(context.Background(), "new", newSet) }()
	select {
	case <-retirementEntered:
	case <-time.After(time.Second):
		t.Fatal("rollback did not restore and enqueue the candidate")
	}
	select {
	case err := <-done:
		t.Fatalf("rollback returned while retirement was blocked: %v", err)
	default:
	}
	candidateLease.Release()
	close(allowRetirement)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("rollback did not finish after candidate lease released")
	}
	predecessorLease.Release()
}

func TestGenerationEngineRollbackIsNotBlockedByUnrelatedRetirement(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	setA := prepareEngineGeneration(t, fixture.engine, 74, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "a", setA)
	leaseA, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("generation A lease unavailable")
	}
	cleanupAStarted := make(chan struct{})
	cleanupCStarted := make(chan struct{})
	var cleanupSequence atomic.Int64
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage != "before-owner-retirement" {
			return nil
		}
		switch cleanupSequence.Add(1) {
		case 1:
			close(cleanupAStarted)
		case 2:
			close(cleanupCStarted)
		}
		return nil
	}
	fixture.engine.mu.Unlock()

	setB := prepareEngineGeneration(t, fixture.engine, 75, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "b", setB)
	select {
	case <-cleanupAStarted:
	case <-time.After(time.Second):
		leaseA.Release()
		t.Fatal("generation A retirement cleanup did not start")
	}

	setC := prepareEngineGeneration(t, fixture.engine, 76, generation.DomainHTTP)
	activateEngineGeneration(t, fixture.engine, "c", setC)
	rollbackDone := make(chan error, 1)
	go func() {
		rollbackDone <- fixture.engine.RollbackActivation(context.Background(), "c", setC)
	}()
	select {
	case <-cleanupCStarted:
	case <-time.After(time.Second):
		leaseA.Release()
		<-rollbackDone
		t.Fatal("generation C cleanup was head-of-line blocked by generation A lease")
	}
	select {
	case err := <-rollbackDone:
		if err != nil {
			leaseA.Release()
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		leaseA.Release()
		t.Fatal("rollback did not finish after generation C cleanup started")
	}
	leaseA.Release()
}

func TestGenerationEngineFinalizeReturnsBeforePredecessorLeaseDrains(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	oldSet := prepareEngineGeneration(t, fixture.engine, 34, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
	oldOwner := fixture.engine.active.Load().http
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("predecessor HTTP lease unavailable")
	}
	newSet := prepareEngineGeneration(t, fixture.engine, 35, generation.DomainHTTP)
	activateEngineGeneration(t, fixture.engine, "new", newSet)
	fixture.engine.FinalizeActivation(context.Background(), "new", newSet)
	select {
	case <-oldOwner.closeDone:
		t.Fatal("finalize closed predecessor while its lease remained live")
	default:
	}
	lease.Release()
	select {
	case <-oldOwner.closeDone:
	case <-time.After(time.Second):
		t.Fatal("predecessor did not retire after lease release")
	}
}

func TestGenerationEngineFinalizeRetiresReplacedOwnerExactlyOnce(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	oldSet := prepareEngineGeneration(t, fixture.engine, 36, generation.DomainHTTP, generation.DomainStream)
	finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
	oldOwner := fixture.engine.active.Load().http
	newSet := prepareEngineGeneration(t, fixture.engine, 37, generation.DomainHTTP, generation.DomainStream)
	finalizeEngineGeneration(t, fixture.engine, "new", newSet)
	select {
	case <-oldOwner.closeDone:
	case <-time.After(time.Second):
		t.Fatal("replaced composite owner did not retire")
	}
	if oldOwner.prepared.HTTP() != nil || oldOwner.prepared.Stream() != nil {
		t.Fatal("retired owner retained protocol snapshots")
	}
	if err := oldOwner.closePrepared(context.Background()); err != oldOwner.closeErr {
		t.Fatalf("close replay = %v, want first result %v", err, oldOwner.closeErr)
	}
}

func TestGenerationEngineConfirmActiveChecksRequestedSubsetOnly(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	httpSet := prepareEngineGeneration(t, fixture.engine, 38, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "http", httpSet)
	streamSet := prepareEngineGeneration(t, fixture.engine, 39, generation.DomainStream)
	finalizeEngineGeneration(t, fixture.engine, "stream", streamSet)
	if err := fixture.engine.ConfirmActive(context.Background(), httpSet); err != nil {
		t.Fatalf("ConfirmActive(HTTP subset) error = %v", err)
	}
	if err := fixture.engine.ConfirmActive(context.Background(), streamSet); err != nil {
		t.Fatalf("ConfirmActive(stream subset) error = %v", err)
	}
}

func TestGenerationEngineConfirmActiveZeroDomainRequiresInitializedFence(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	_, empty := generationEngineInput(t, 40)
	zero := generation.PublicationSet{
		DesiredRevision: empty.Revision(),
		Domains:         map[generation.Domain]generation.PublicationCandidate{},
	}
	if err := fixture.engine.ConfirmActive(
		context.Background(),
		zero,
	); !errors.Is(
		err,
		generation.ErrActiveGenerationMismatch,
	) {
		t.Fatalf("empty-journal ConfirmActive() error = %v", err)
	}
	set := prepareEngineGeneration(t, fixture.engine, 40)
	finalizeEngineGeneration(t, fixture.engine, "zero", set)
	if err := fixture.engine.ConfirmActive(context.Background(), set); err != nil {
		t.Fatalf("committed zero-domain ConfirmActive() error = %v", err)
	}
}

func TestGenerationEngineCommittedReplayDoesNotCompileOrMutate(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 41, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "http", set)
	active := fixture.engine.active.Load()
	if err := fixture.engine.ConfirmActive(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	if fixture.engine.active.Load() != active || len(fixture.engine.pending) != 0 ||
		len(fixture.engine.activations) != 0 {
		t.Fatal("committed replay compiled or mutated engine state")
	}
}

func TestGenerationEngineConcurrentCloseAndReleaseReplaysFirstCleanupError(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 42, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "http", set)
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("HTTP lease unavailable")
	}
	retirementEntered := make(chan struct{})
	allowRetirement := make(chan struct{})
	var retirementOnce sync.Once
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			retirementOnce.Do(func() {
				close(retirementEntered)
				<-allowRetirement
			})
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	errorsSeen := make(chan error, 2)
	go func() { errorsSeen <- fixture.engine.Close(context.Background()) }()
	go func() { errorsSeen <- fixture.engine.Close(context.Background()) }()
	select {
	case <-retirementEntered:
	case <-time.After(time.Second):
		t.Fatal("Close() did not enqueue the active owner")
	}
	select {
	case err := <-errorsSeen:
		t.Fatalf("Close() returned before retirement barrier release: %v", err)
	default:
	}
	lease.Release()
	close(allowRetirement)
	first, second := <-errorsSeen, <-errorsSeen
	if first != second {
		t.Fatalf("Close() replay errors differ: %v / %v", first, second)
	}
}

func TestGenerationEngineAcquisitionRetriesChangedBundle(t *testing.T) {
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		t.Run(string(domain), func(t *testing.T) {
			fixture := newGenerationEngineFixture(t)
			oldSet := prepareEngineGeneration(t, fixture.engine, 43, domain)
			finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
			loaded := make(chan struct{})
			resume := make(chan struct{})
			var loadedOnce sync.Once
			fixture.engine.mu.Lock()
			fixture.engine.checkpoint = func(stage string) error {
				if stage == string(domain)+"-bundle-loaded" {
					loadedOnce.Do(func() {
						close(loaded)
						<-resume
					})
				}
				return nil
			}
			fixture.engine.mu.Unlock()
			acquired := make(chan any, 1)
			go func() {
				if domain == generation.DomainHTTP {
					lease, ok := fixture.engine.acquireHTTP()
					if !ok {
						acquired <- nil
						return
					}
					defer lease.Release()
					acquired <- lease.Snapshot
					return
				}
				lease, ok := fixture.engine.acquireStream()
				if !ok {
					acquired <- nil
					return
				}
				defer lease.Release()
				acquired <- lease.Router
			}()
			select {
			case <-loaded:
			case <-time.After(time.Second):
				t.Fatal("acquisition did not reach bundle-loaded checkpoint")
			}
			newSet := prepareEngineGeneration(t, fixture.engine, 44, domain)
			finalizeEngineGeneration(t, fixture.engine, "new", newSet)
			newOwner := fixture.engine.active.Load().http
			if domain == generation.DomainStream {
				newOwner = fixture.engine.active.Load().stream
			}
			close(resume)
			got := <-acquired
			if domain == generation.DomainHTTP {
				if got != newOwner.prepared.HTTP() {
					t.Fatal("HTTP acquisition did not retry onto the new bundle snapshot")
				}
			} else if got != newOwner.prepared.Stream().Router() {
				t.Fatal("stream acquisition did not retry onto the new bundle router")
			}
		})
	}
}

func TestGenerationEngineRollsBackActivatedGenerationWhenJournalCommitFails(t *testing.T) {
	for _, test := range []struct {
		name           string
		checkpointErr  error
		commitErr      error
		wantCommitCall int
	}{
		{name: "commit failure", commitErr: errors.New("commit failed"), wantCommitCall: 1},
		{name: "activation checkpoint failure", checkpointErr: errors.New("activation failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGenerationEngineFixture(t)
			oldSet := prepareEngineGeneration(t, fixture.engine, 50, generation.DomainHTTP, generation.DomainStream)
			finalizeEngineGeneration(t, fixture.engine, "old", oldSet)
			previous := fixture.engine.active.Load()
			ticket, desired := generationEngineInput(t, 51, generation.DomainHTTP, generation.DomainStream)
			journal := &engineCommitFailureJournal{
				ticket: ticket, desired: desired, previous: publishedFromEngineSet(oldSet),
				commitErr: test.commitErr, abortErr: errors.New("abort failed"),
			}
			var rejected *generationOwner
			fixture.engine.mu.Lock()
			fixture.engine.checkpoint = func(stage string) error {
				if stage == "candidate-bundle-published" {
					rejected = fixture.engine.active.Load().http
					return test.checkpointErr
				}
				return nil
			}
			fixture.engine.mu.Unlock()
			coordinator := generation.NewCoordinator(journal, fixture.engine)
			_, err := coordinator.Apply(context.Background(), generation.DesiredBatch{
				Cursor: ticket.Cursor, RequiredDomains: slices.Clone(ticket.RequiredDomains),
			})
			primary := test.commitErr
			if primary == nil {
				primary = test.checkpointErr
			}
			if !errors.Is(err, primary) || !errors.Is(err, journal.abortErr) {
				t.Fatalf("Coordinator.Apply() error = %v, want primary and abort errors", err)
			}
			if journal.commitCalls != test.wantCommitCall || journal.abortToken != "staged" {
				t.Fatalf("journal calls commit=%d abort=%q", journal.commitCalls, journal.abortToken)
			}
			if active := fixture.engine.active.Load(); active.http != previous.http ||
				active.stream != previous.stream {
				t.Fatal("coordinator failure did not restore predecessor bundle")
			}
			if rejected == nil {
				t.Fatal("activation never published a tentative candidate")
			}
			select {
			case <-rejected.closeDone:
			default:
				t.Fatal("coordinator failure returned before rejected owner closed")
			}
		})
	}
}

type engineCommitFailureJournal struct {
	ticket      generation.ApplyTicket
	desired     generation.Snapshot
	previous    map[generation.Domain]generation.PublishedGeneration
	commitErr   error
	abortErr    error
	commitCalls int
	abortToken  generation.PublicationToken
}

func (*engineCommitFailureJournal) LoadAcknowledgement(
	context.Context,
	generation.ProviderCursor,
) (generation.Acknowledgement, error) {
	return generation.Acknowledgement{}, generation.ErrNotFound
}

func (journal *engineCommitFailureJournal) ApplyDesired(
	context.Context,
	generation.DesiredBatch,
) (generation.ApplyTicket, error) {
	return journal.ticket, nil
}

func (journal *engineCommitFailureJournal) LoadDesired(
	context.Context,
	uint64,
) (generation.Snapshot, error) {
	return journal.desired.Clone(), nil
}

func (journal *engineCommitFailureJournal) LoadPublished(
	_ context.Context,
	domain generation.Domain,
) (generation.PublishedGeneration, error) {
	published, ok := journal.previous[domain]
	if !ok {
		return generation.PublishedGeneration{}, generation.ErrNotFound
	}
	return published, nil
}

func (*engineCommitFailureJournal) Stage(
	context.Context,
	generation.ApplyTicket,
	generation.PublicationSet,
) (generation.PublicationToken, error) {
	return "staged", nil
}

func (journal *engineCommitFailureJournal) Commit(
	context.Context,
	generation.PublicationToken,
) (generation.Acknowledgement, error) {
	journal.commitCalls++
	return generation.Acknowledgement{}, journal.commitErr
}

func (journal *engineCommitFailureJournal) Abort(
	_ context.Context,
	token generation.PublicationToken,
	_ string,
) error {
	journal.abortToken = token
	return journal.abortErr
}

func (*engineCommitFailureJournal) Revisions(context.Context) (generation.RevisionSet, error) {
	return generation.RevisionSet{}, nil
}

func (*engineCommitFailureJournal) Recover(context.Context) (generation.RecoveryState, error) {
	return generation.RecoveryState{}, nil
}

func (*engineCommitFailureJournal) Close() error { return nil }

func publishedFromEngineSet(
	set generation.PublicationSet,
) map[generation.Domain]generation.PublishedGeneration {
	published := make(map[generation.Domain]generation.PublishedGeneration, len(set.Domains))
	for domain, candidate := range set.Domains {
		published[domain] = generation.PublishedGeneration(candidate)
	}
	return published
}

func TestGenerationEngineInstallRecoveryUsesPublishedNotDesired(t *testing.T) {
	source := newGenerationEngineFixture(t)
	publishedSet := prepareEngineGeneration(t, source.engine, 60, generation.DomainHTTP, generation.DomainStream)
	published := publishedFromEngineSet(publishedSet)
	desired, err := generation.NewSnapshot(61, []generation.Resource{{
		Key:   generation.ResourceKey{Kind: "routes", ID: "desired-marker-a"},
		Value: []byte(`{"id":"desired-marker-a","plugins":{"not-a-plugin":{}}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := newUnrecoveredGenerationEngineFixture(t)
	state := generation.RecoveryState{
		Revisions: generation.RevisionSet{Desired: 61, HTTP: 60, Stream: 60},
		Desired:   desired,
		Published: published,
	}
	if err := target.engine.InstallRecovery(context.Background(), state); err != nil {
		t.Fatalf("InstallRecovery() error = %v; desired-only marker was incorrectly compiled: %v", err, err)
	}
	if target.engine.fences[generation.DomainHTTP].Artifact != publishedSet.Domains[generation.DomainHTTP].Artifact ||
		target.engine.fences[generation.DomainStream].Artifact != publishedSet.Domains[generation.DomainStream].Artifact {
		t.Fatal("recovery fences were not cloned from Published")
	}
}

func TestGenerationEngineStreamMetricsPublishRecoveredRoutes(t *testing.T) {
	installGenerationStreamMetrics(t)
	source := newGenerationEngineFixture(t)
	publishedSet := prepareEngineStreamGeneration(t, source.engine, 84, "recovered")
	target := newUnrecoveredGenerationEngineFixture(t)
	if err := target.engine.InstallRecovery(context.Background(), generation.RecoveryState{
		Revisions: generation.RevisionSet{Desired: 84, Stream: 84},
		Desired:   publishedSet.Domains[generation.DomainStream].Snapshot,
		Published: publishedFromEngineSet(publishedSet),
	}); err != nil {
		t.Fatal(err)
	}
	assertGenerationStreamMetricDelta(t, "recovered", 1)
}

func TestGenerationEngineStreamMetricsClearOnlyAfterTerminalDrain(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineStreamGeneration(t, fixture.engine, 85, "closing")
	finalizeEngineGeneration(t, fixture.engine, "stream", set)
	lease, ok := fixture.engine.acquireStream()
	if !ok {
		t.Fatal("active stream lease unavailable")
	}
	t.Cleanup(lease.Release)

	retirementEntered := make(chan struct{})
	allowRetirement := make(chan struct{})
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			close(retirementEntered)
			<-allowRetirement
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- fixture.engine.Close(context.Background()) }()
	select {
	case <-retirementEntered:
	case <-time.After(time.Second):
		lease.Release()
		t.Fatal("terminal close did not enter stream owner retirement")
	}
	assertGenerationStreamMetricDelta(t, "closing", 1)
	lease.Release()
	close(allowRetirement)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal close did not finish after stream lease drained")
	}
	assertGenerationStreamMetricDelta(t, "closing", 0)
}

func TestGenerationEngineInstallRecoveryPreservesIndependentDomainRevisions(t *testing.T) {
	source := newGenerationEngineFixture(t)
	httpSet := prepareEngineGeneration(t, source.engine, 62, generation.DomainHTTP)
	streamSet := prepareEngineGeneration(t, source.engine, 63, generation.DomainStream)
	published := publishedFromEngineSet(httpSet)
	maps.Copy(published, publishedFromEngineSet(streamSet))
	desired, err := generation.NewSnapshot(64, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := newUnrecoveredGenerationEngineFixture(t)
	if err := target.engine.InstallRecovery(context.Background(), generation.RecoveryState{
		Revisions: generation.RevisionSet{Desired: 64, HTTP: 62, Stream: 63},
		Desired:   desired,
		Published: published,
	}); err != nil {
		t.Fatal(err)
	}
	active := target.engine.active.Load()
	if active.http == nil || active.http != active.stream ||
		target.engine.fences[generation.DomainHTTP].Artifact.Revision != 62 ||
		target.engine.fences[generation.DomainStream].Artifact.Revision != 63 {
		t.Fatalf("independent recovery bundle/fences = %#v / %#v", active, target.engine.fences)
	}
}

func TestGenerationEngineRecoveryPublicationIdentityUsesPublishedDefensively(t *testing.T) {
	source := newGenerationEngineFixture(t)
	httpSet := prepareEngineGeneration(t, source.engine, 77, generation.DomainHTTP)
	streamSet := prepareEngineGeneration(t, source.engine, 78, generation.DomainStream)
	published := publishedFromEngineSet(httpSet)
	maps.Copy(published, publishedFromEngineSet(streamSet))
	revisions := generation.RevisionSet{Desired: 79, HTTP: 77, Stream: 78}

	identity := recoveryPublicationSet(revisions, published)
	if identity.DesiredRevision != 79 ||
		identity.Domains[generation.DomainHTTP].Artifact.Revision != 77 ||
		identity.Domains[generation.DomainStream].Artifact.Revision != 78 {
		t.Fatalf("recovery publication identity = %#v", identity)
	}
	streamPublished := published[generation.DomainStream]
	streamPublished.Decisions[0].Code = "caller-mutated"
	published[generation.DomainStream] = streamPublished
	if identity.Domains[generation.DomainStream].Decisions[0].Code == "caller-mutated" {
		t.Fatal("recovery publication identity aliases caller Published decisions")
	}
}

func TestGenerationEngineInstallRecoveryLeavesMissingDomainUnavailable(t *testing.T) {
	source := newGenerationEngineFixture(t)
	httpSet := prepareEngineGeneration(t, source.engine, 65, generation.DomainHTTP)
	desired, err := generation.NewSnapshot(66, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := newUnrecoveredGenerationEngineFixture(t)
	if err := target.engine.InstallRecovery(context.Background(), generation.RecoveryState{
		Revisions: generation.RevisionSet{Desired: 66, HTTP: 65},
		Desired:   desired,
		Published: publishedFromEngineSet(httpSet),
		Failures:  []generation.RecoveryFailure{{Domain: generation.DomainStream, Code: "damaged"}},
	}); err != nil {
		t.Fatal(err)
	}
	httpLease, ok := target.engine.acquireHTTP()
	if !ok {
		t.Fatal("published HTTP recovery was unavailable")
	}
	httpLease.Release()
	if _, ok := target.engine.acquireStream(); ok {
		t.Fatal("missing stream recovery became available")
	}
}

func TestGenerationEngineInstallEmptyJournalLeavesReplayUninitialized(t *testing.T) {
	fixture := newUnrecoveredGenerationEngineFixture(t)
	if err := fixture.engine.InstallRecovery(context.Background(), generation.RecoveryState{}); err != nil {
		t.Fatal(err)
	}
	set := generation.PublicationSet{
		DesiredRevision: 67,
		Domains:         map[generation.Domain]generation.PublicationCandidate{},
	}
	if err := fixture.engine.ConfirmActive(
		context.Background(),
		set,
	); !errors.Is(
		err,
		generation.ErrActiveGenerationMismatch,
	) {
		t.Fatalf("ConfirmActive() error = %v, want uninitialized mismatch", err)
	}
}

func TestGenerationEngineInstallDesiredOnlyRecoveryInitializesZeroDomainReplay(t *testing.T) {
	fixture := newUnrecoveredGenerationEngineFixture(t)
	desired, err := generation.NewSnapshot(68, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.InstallRecovery(context.Background(), generation.RecoveryState{
		Revisions: generation.RevisionSet{Desired: 68}, Desired: desired,
	}); err != nil {
		t.Fatal(err)
	}
	set := generation.PublicationSet{
		DesiredRevision: 68,
		Domains:         map[generation.Domain]generation.PublicationCandidate{},
	}
	if err := fixture.engine.ConfirmActive(context.Background(), set); err != nil {
		t.Fatalf("desired-only zero-domain replay error = %v", err)
	}
}

func TestGenerationEngineRejectsSecondRecoveryInstall(t *testing.T) {
	fixture := newUnrecoveredGenerationEngineFixture(t)
	if err := fixture.engine.InstallRecovery(context.Background(), generation.RecoveryState{}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.engine.InstallRecovery(
		context.Background(),
		generation.RecoveryState{},
	); !errors.Is(
		err,
		errGenerationRecoveryAlreadyInstalled,
	) {
		t.Fatalf("second InstallRecovery() error = %v", err)
	}
}

func TestGenerationEngineCloseRejectsNewWorkAndClosesOwnersBeforeFactory(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	activeSet := prepareEngineGeneration(t, fixture.engine, 69, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "active", activeSet)
	owner := fixture.engine.active.Load().http
	pendingSet := prepareEngineGeneration(t, fixture.engine, 70, generation.DomainStream)
	pending := fixture.engine.pending[mustEnginePreparedKey(t, pendingSet)].owner
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if owner.prepared.HTTP() != nil || pending.prepared.Stream() != nil {
		t.Fatal("Close() retained an active or pending prepared owner")
	}
	ticket, desired := generationEngineInput(t, 71, generation.DomainHTTP)
	if _, err := fixture.engine.Prepare(
		context.Background(),
		ticket,
		desired,
		nil,
	); !errors.Is(
		err,
		errGenerationEngineClosed,
	) {
		t.Fatalf("Prepare() after Close error = %v", err)
	}
	if _, err := fixture.factory.PrepareGeneration(
		context.Background(),
		ticket,
		desired,
		nil,
		nil,
	); !errors.Is(
		err,
		compiler.ErrWorkerCompilerFactoryClosed,
	) {
		t.Fatalf("factory after engine Close error = %v", err)
	}
	if _, ok := fixture.engine.acquireHTTP(); ok {
		t.Fatal("HTTP acquisition succeeded after Close")
	}
}

func TestGenerationEngineCloseReplaysJoinedOwnerAndFactoryErrors(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("owned registration close failed")
	materializer := &engineErrorMaterializer{digest: catalog.Digest(), closeErr: closeFailure}
	engine, factory := newGenerationEngineWithMaterializer(t, materializer)
	set := prepareEngineGeneration(t, engine, 72, generation.DomainHTTP)
	finalizeEngineGeneration(t, engine, "active", set)
	rogueTicket, rogueDesired := generationEngineInput(t, 73, generation.DomainHTTP)
	if _, err := factory.PrepareGeneration(context.Background(), rogueTicket, rogueDesired, nil, nil); err != nil {
		t.Fatalf("prepare factory-owned cleanup sentinel: %v", err)
	}
	first := engine.Close(context.Background())
	second := engine.Close(context.Background())
	if first != second {
		t.Fatalf("Close() replay errors differ: %v / %v", first, second)
	}
	if first == nil || !strings.Contains(first.Error(), "prepared generation cleanup failed") ||
		!strings.Contains(first.Error(), "worker compiler factory cleanup failed") {
		t.Fatalf("Close() error = %v, want joined owner and factory cleanup errors", first)
	}
}

func TestGenerationEngineRetirementResidualRetainsOwnerAndStreamMetrics(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	setA := prepareEngineStreamGeneration(t, fixture.engine, 86, "retained")
	finalizeEngineGeneration(t, fixture.engine, "retained-a", setA)
	owner := fixture.engine.active.Load().stream
	release := startGenerationEngineBlockingTask(t, owner, "retirement-retained")
	defer release()

	retirementStarted := make(chan struct{})
	var startedOnce sync.Once
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			startedOnce.Do(func() { close(retirementStarted) })
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	setB := prepareEngineStreamGeneration(t, fixture.engine, 87, "replacement")
	finalizeEngineGeneration(t, fixture.engine, "retained-b", setB)
	select {
	case <-retirementStarted:
	case <-time.After(time.Second):
		t.Fatal("retirement did not start")
	}
	fixture.engine.retireMu.Lock()
	attempt := fixture.engine.retireActive[owner]
	fixture.engine.retireMu.Unlock()
	if attempt == nil {
		t.Fatal("retirement attempt is not active")
	}
	attempt.cancel()
	select {
	case <-attempt.done:
	case <-time.After(time.Second):
		t.Fatal("canceled retirement did not publish its residual")
	}
	var residual *runtime.TaskResidualError
	if !errors.As(attempt.err, &residual) {
		t.Fatalf("retirement attempt error = %v", attempt.err)
	}
	fixture.engine.retireMu.Lock()
	_, retained := fixture.engine.retireKnown[owner]
	_, active := fixture.engine.retireActive[owner]
	fixture.engine.retireMu.Unlock()
	fixture.engine.streamMetricsMu.Lock()
	_, metricsRetained := fixture.engine.streamMetricOwners[owner]
	fixture.engine.streamMetricsMu.Unlock()
	if !retained || active || !metricsRetained {
		t.Fatalf("retirement state retained=%t active=%t metrics=%t", retained, active, metricsRetained)
	}

	release()
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatalf("Close() retry error = %v", err)
	}
	fixture.engine.retireMu.Lock()
	_, retained = fixture.engine.retireKnown[owner]
	fixture.engine.retireMu.Unlock()
	fixture.engine.streamMetricsMu.Lock()
	_, metricsRetained = fixture.engine.streamMetricOwners[owner]
	fixture.engine.streamMetricsMu.Unlock()
	if retained || metricsRetained {
		t.Fatal("terminal retirement retained owner or stream metrics")
	}
}

func TestGenerationEngineCloseCancelsActiveRetirementAttemptBeforeRetry(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	setA := prepareEngineStreamGeneration(t, fixture.engine, 88, "cancel-active")
	finalizeEngineGeneration(t, fixture.engine, "cancel-a", setA)
	owner := fixture.engine.active.Load().stream
	release := startGenerationEngineBlockingTask(t, owner, "retirement-cancel")
	defer release()
	retirementStarted := make(chan struct{})
	var startedOnce sync.Once
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		if stage == "before-owner-retirement" {
			startedOnce.Do(func() { close(retirementStarted) })
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	setB := prepareEngineStreamGeneration(t, fixture.engine, 89, "replacement")
	finalizeEngineGeneration(t, fixture.engine, "cancel-b", setB)
	select {
	case <-retirementStarted:
	case <-time.After(time.Second):
		t.Fatal("retirement did not start")
	}

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	first := fixture.engine.Close(short)
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) {
		t.Fatalf("first Close() error = %v", first)
	}
	fixture.engine.retireMu.Lock()
	_, retained := fixture.engine.retireKnown[owner]
	_, active := fixture.engine.retireActive[owner]
	fixture.engine.retireMu.Unlock()
	if !retained || active || owner.prepared == nil {
		t.Fatal("Close did not join the canceled attempt before retaining its owner")
	}

	release()
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
}

func TestGenerationEngineCloseDeadlineReturnsWithoutWaitingForever(t *testing.T) {
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineGeneration(t, fixture.engine, 90, generation.DomainHTTP)
	finalizeEngineGeneration(t, fixture.engine, "deadline", set)
	owner := fixture.engine.active.Load().http
	lease, ok := fixture.engine.acquireHTTP()
	if !ok {
		t.Fatal("HTTP lease unavailable")
	}
	defer lease.Release()

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := time.Now()
	first := fixture.engine.Close(short)
	if first == nil || (!errors.Is(first, context.Canceled) && !errors.Is(first, context.DeadlineExceeded)) {
		t.Fatalf("Close() error = %v, want bounded context error", first)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Close() exceeded bounded wait: %s", elapsed)
	}
	if !fixture.engine.closed || owner.prepared == nil || fixture.engine.closePhase == engineCloseFactoryDone {
		t.Fatal("bounded Close released retained ownership or failed to seal the engine")
	}
	ticket, desired := generationEngineInput(t, 91, generation.DomainHTTP)
	_, prepareErr := fixture.engine.Prepare(context.Background(), ticket, desired, nil)
	if !errors.Is(prepareErr, errGenerationEngineClosed) {
		t.Fatalf("Prepare() after bounded Close error = %v", prepareErr)
	}

	lease.Release()
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
}

func TestGenerationEngineCloseRetryClosesPendingRetirementsThenFactory(t *testing.T) {
	installGenerationStreamMetrics(t)
	fixture := newGenerationEngineFixture(t)
	set := prepareEngineStreamGeneration(t, fixture.engine, 92, "ordered")
	finalizeEngineGeneration(t, fixture.engine, "ordered", set)
	owner := fixture.engine.active.Load().stream
	release := startGenerationEngineBlockingTask(t, owner, "close-order")
	defer release()

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	first := fixture.engine.Close(short)
	cancel()
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) {
		t.Fatalf("first Close() error = %v", first)
	}
	release()
	var traceMu sync.Mutex
	var trace []string
	fixture.engine.mu.Lock()
	fixture.engine.checkpoint = func(stage string) error {
		switch stage {
		case "owner-terminal", "metrics-unregister", "factory-close":
			traceMu.Lock()
			trace = append(trace, stage)
			traceMu.Unlock()
		}
		return nil
	}
	fixture.engine.mu.Unlock()
	if err := fixture.engine.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	traceMu.Lock()
	got := slices.Clone(trace)
	traceMu.Unlock()
	want := []string{"owner-terminal", "metrics-unregister", "factory-close"}
	if !slices.Equal(got, want) {
		t.Fatalf("close trace = %v, want %v", got, want)
	}
}

func TestGenerationEngineClosePreservesIndependentCleanupErrorsAcrossRetry(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &engineErrorMaterializer{
		digest: catalog.Digest(), closeErr: errors.New("terminal cleanup fixture"),
	}
	engine, _ := newGenerationEngineWithMaterializer(t, materializer)
	terminalSet := prepareEngineGeneration(t, engine, 93, generation.DomainHTTP)
	terminalRecord := engine.pending[mustEnginePreparedKey(t, terminalSet)]
	terminalErr := terminalRecord.owner.prepared.Close(context.Background())
	if terminalErr == nil {
		t.Fatal("terminal fixture cleanup did not fail")
	}
	retrySet := prepareEngineGeneration(t, engine, 94, generation.DomainHTTP)
	retryRecord := engine.pending[mustEnginePreparedKey(t, retrySet)]
	release := startGenerationEngineBlockingTask(t, retryRecord.owner, "independent-error")
	defer release()

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	first := engine.Close(short)
	cancel()
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, terminalErr) {
		t.Fatalf("first Close() error = %v, want residual plus %v", first, terminalErr)
	}
	release()
	finalErr := engine.Close(context.Background())
	if !errors.Is(finalErr, terminalErr) {
		t.Fatalf("terminal Close() error = %v, want retained %v", finalErr, terminalErr)
	}
}

func startGenerationEngineBlockingTask(
	t *testing.T,
	owner *generationOwner,
	name string,
) func() {
	t.Helper()
	if owner == nil || owner.prepared == nil {
		t.Fatal("blocking task requires a live generation owner")
	}
	tasks := generationOwnerTaskRegistry(t, owner.prepared)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	if err := tasks.Go(runtime.TaskSpec{
		Owner: "plugin/test/" + name, Criticality: runtime.TaskPlugin,
	}, func(context.Context) error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatalf("TaskRegistry.Go() error = %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocking generation task did not start")
	}
	return func() { releaseOnce.Do(func() { close(release) }) }
}

type engineErrorMaterializer struct {
	digest       [32]byte
	closeErr     error
	closeStarted chan struct{}
	closeProceed chan struct{}
	last         *engineErrorRegistration
}

func (materializer *engineErrorMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	registration := &engineErrorRegistration{
		id: secret.CandidateAttemptID(ticket, set), closeErr: materializer.closeErr,
		closeStarted: materializer.closeStarted, closeProceed: materializer.closeProceed,
	}
	materializer.last = registration
	return registration, nil
}

func (materializer *engineErrorMaterializer) RegisterRecovery(
	_ context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	return &engineErrorRegistration{
		id:       secret.RecoveryAttemptID(revisions, published),
		closeErr: materializer.closeErr,
	}, nil
}

func (materializer *engineErrorMaterializer) DeclarationDigest() [32]byte { return materializer.digest }

type engineErrorRegistration struct {
	id           secret.AttemptID
	closeErr     error
	closeStarted chan struct{}
	closeProceed chan struct{}
	closeCalls   atomic.Int64
}

func (registration *engineErrorRegistration) AttemptID() secret.AttemptID { return registration.id }

func (*engineErrorRegistration) Materialize(
	context.Context,
	secret.Scope,
	string,
) (secret.Value, error) {
	return secret.Value{}, secret.ErrCredentialUnavailable
}

func (registration *engineErrorRegistration) Close(context.Context) error {
	registration.closeCalls.Add(1)
	if registration.closeStarted != nil {
		close(registration.closeStarted)
	}
	if registration.closeProceed != nil {
		<-registration.closeProceed
	}
	return registration.closeErr
}

func newGenerationEngineWithMaterializer(
	t *testing.T,
	materializer secret.Materializer,
) (*GenerationEngine, *compiler.WorkerCompilerFactory) {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	profiles := config.ProfileSelection{
		Compatibility: config.CompatibilityTarget(manifest.Target.Name), Security: config.SecurityCompat,
	}
	effective := &config.EffectiveConfig{
		Config: config.Config{
			CompatibilityTarget: profiles.Compatibility, SecurityProfile: profiles.Security,
			QualificationProfile: profiles.Qualification,
		},
		Profiles: profiles,
	}
	factory, err := compiler.NewWorkerCompilerFactory(
		manifest, effective, materializer,
		compiler.WorkerRuntimeObservers{Cluster: proxy.NopClusterObserver{}, Stream: func(streamruntime.Result) {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewGenerationEngine(&Server{}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.InstallRecovery(context.Background(), generation.RecoveryState{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	return engine, factory
}
