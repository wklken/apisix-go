package server

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/compiler"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func TestActiveBundlePreservesUntouchedDomainOwner(t *testing.T) {
	old := newTestGenerationOwner(t, 40, true, true)
	nextHTTP := newTestGenerationOwner(t, 41, true, false)
	bundle := activeBundle{http: old.owner, stream: old.owner}

	replaced := bundle.withDomains(nextHTTP.owner, ownerDomainHTTP)

	if replaced.http != nextHTTP.owner {
		t.Fatal("HTTP owner was not replaced")
	}
	if replaced.stream != old.owner {
		t.Fatal("HTTP-only replacement changed the stream owner")
	}
	if bundle.http != old.owner || bundle.stream != old.owner {
		t.Fatal("withDomains mutated the published bundle")
	}
}

func TestGenerationOwnerHTTPRetirementDoesNotRetireActiveStreamSlot(t *testing.T) {
	fixture := newTestGenerationOwner(t, 41, true, true)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP | ownerDomainStream)
	httpSource := httpLeaseSource(owner.acquireHTTP)
	streamSource := streamLeaseSource(owner.acquireStream)
	httpLease, ok := httpSource()
	if !ok {
		t.Fatal("acquireHTTP() = false")
	}
	if retired := owner.deactivateDomains(ownerDomainHTTP); retired {
		t.Fatal("HTTP deactivation retired an active stream owner")
	}
	if _, ok := owner.acquireHTTP(); ok {
		t.Fatal("stale HTTP acquisition succeeded")
	}
	streamLease, ok := streamSource()
	if !ok {
		t.Fatal("stream slot was retired with HTTP")
	}
	httpLease.Release()
	streamLease.Release()
	assertNotDrained(t, owner)
	if retired := owner.deactivateDomains(ownerDomainStream); !retired {
		t.Fatal("final stream deactivation did not retire owner")
	}
	assertDrained(t, owner)
}

func TestGenerationOwnerLeaseReleaseIsExactlyOnce(t *testing.T) {
	fixture := newTestGenerationOwner(t, 42, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	lease, ok := owner.acquireHTTP()
	if !ok {
		t.Fatal("acquireHTTP() = false")
	}
	lease.Release()
	lease.Release()
	if retired := owner.deactivateDomains(ownerDomainHTTP); !retired {
		t.Fatal("HTTP deactivation did not retire owner")
	}
	assertDrained(t, owner)
}

func TestGenerationOwnerInvalidDomainTransitionsFailLoud(t *testing.T) {
	t.Run("activate already active domain", func(t *testing.T) {
		owner := newTestGenerationOwner(t, 46, true, false).owner
		owner.activateDomains(ownerDomainHTTP)
		assertPanics(t, func() { owner.activateDomains(ownerDomainHTTP) })
	})

	t.Run("activate missing snapshot", func(t *testing.T) {
		owner := newTestGenerationOwner(t, 47, true, false).owner
		assertPanics(t, func() { owner.activateDomains(ownerDomainStream) })
	})

	t.Run("deactivate inactive domain", func(t *testing.T) {
		owner := newTestGenerationOwner(t, 48, true, true).owner
		owner.activateDomains(ownerDomainHTTP)
		assertPanics(t, func() { owner.deactivateDomains(ownerDomainStream) })
	})
}

func TestGenerationOwnerRetainedHTTPLeasePinsSameSnapshot(t *testing.T) {
	fixture := newTestGenerationOwner(t, 43, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	parent, ok := owner.acquireHTTP()
	if !ok {
		t.Fatal("acquireHTTP() = false")
	}
	if retired := owner.deactivateDomains(ownerDomainHTTP); !retired {
		t.Fatal("HTTP deactivation did not retire owner")
	}
	child, ok := parent.retain()
	if !ok {
		t.Fatal("retain() rejected a live parent lease after slot replacement")
	}
	if child.Snapshot != parent.Snapshot {
		t.Fatal("retain() did not pin the parent's exact HTTP snapshot")
	}
	parent.Release()
	assertNotDrained(t, owner)
	child.Release()
	assertDrained(t, owner)
	if _, ok := parent.retain(); ok {
		t.Fatal("released parent lease retained another child")
	}
}

func TestGenerationOwnerDoesNotClosePreparedOnDrain(t *testing.T) {
	fixture := newTestGenerationOwner(t, 44, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	owner.deactivateDomains(ownerDomainHTTP)
	select {
	case <-owner.closeDone:
		t.Fatal("close completion signaled before prepared generation closed")
	default:
	}
	assertDrained(t, owner)

	if got := fixture.registration.closeCalls.Load(); got != 0 {
		t.Fatalf("drain closed prepared generation %d times", got)
	}
	if snapshot := fixture.prepared.HTTP(); snapshot == nil || snapshot.Revision() != 44 {
		t.Fatal("drain revoked the prepared HTTP snapshot")
	}
}

func TestGenerationOwnerClosePreparedWaitsForDrainAndClosesOnce(t *testing.T) {
	fixture := newTestGenerationOwner(t, 45, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	lease, ok := owner.acquireHTTP()
	if !ok {
		t.Fatal("acquireHTTP() = false")
	}
	owner.deactivateDomains(ownerDomainHTTP)

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := owner.closePrepared(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closePrepared() before drain error = %v, want deadline exceeded", err)
	}
	if got := fixture.registration.closeCalls.Load(); got != 0 {
		t.Fatalf("timed-out close closed prepared generation %d times", got)
	}

	lease.Release()
	assertDrained(t, owner)
	const callers = 8
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	group.Add(callers)
	for range callers {
		go func() {
			defer group.Done()
			errorsSeen <- owner.closePrepared(context.Background())
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("closePrepared() error = %v", err)
		}
	}
	if got := fixture.registration.closeCalls.Load(); got != 1 {
		t.Fatalf("prepared generation close calls = %d, want 1", got)
	}
	select {
	case <-owner.closeDone:
	default:
		t.Fatal("close completion was not signaled")
	}
	if fixture.prepared.HTTP() != nil {
		t.Fatal("closePrepared() left HTTP snapshot visible")
	}
}

func TestGenerationOwnerTerminalCloseReplaysWithCanceledContext(t *testing.T) {
	fixture := newTestGenerationOwner(t, 51, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	owner.deactivateDomains(ownerDomainHTTP)

	if err := owner.closePrepared(context.Background()); err != nil {
		t.Fatalf("terminal close = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := owner.closePrepared(canceled); err != nil {
		t.Fatalf("terminal replay with canceled context = %v, want cached nil", err)
	}
}

func TestGenerationOwnerPreparedResidualKeepsCloseDoneOpenUntilRetry(t *testing.T) {
	fixture := newTestGenerationOwner(t, 49, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	owner.deactivateDomains(ownerDomainHTTP)

	release := make(chan struct{})
	started := make(chan struct{})
	tasks := generationOwnerTaskRegistry(t, fixture.prepared)
	if err := tasks.Go(
		runtime.TaskSpec{Owner: "server-test/blocking-generation", Criticality: runtime.TaskPlugin},
		func(context.Context) error {
			close(started)
			<-release
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	<-started

	shortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	first := owner.closePrepared(shortCtx)
	var residual *runtime.TaskResidualError
	if !errors.As(first, &residual) || !errors.Is(first, context.DeadlineExceeded) {
		t.Fatalf("first close = %v, residual = %#v", first, residual)
	}
	select {
	case <-owner.closeDone:
		t.Fatal("incomplete prepared close signaled terminal closeDone")
	default:
	}
	if owner.prepared == nil {
		t.Fatal("incomplete close dropped prepared ownership")
	}

	close(release)
	if err := owner.closePrepared(context.Background()); err != nil {
		t.Fatalf("retry close = %v", err)
	}
	select {
	case <-owner.closeDone:
	default:
		t.Fatal("terminal retry did not close closeDone")
	}
	if owner.prepared != nil {
		t.Fatal("terminal close retained prepared owner")
	}
}

func TestGenerationOwnerConcurrentCloseAttemptsSerializeAndHonorCallerContext(t *testing.T) {
	fixture := newTestGenerationOwner(t, 50, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	owner.deactivateDomains(ownerDomainHTTP)

	release := make(chan struct{})
	started := make(chan struct{})
	taskCanceled := make(chan struct{})
	tasks := generationOwnerTaskRegistry(t, fixture.prepared)
	if err := tasks.Go(
		runtime.TaskSpec{Owner: "server-test/concurrent-blocking-generation", Criticality: runtime.TaskPlugin},
		func(taskCtx context.Context) error {
			close(started)
			<-taskCtx.Done()
			close(taskCanceled)
			<-release
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	<-started

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelLeader()
	leaderResult := make(chan error, 1)
	go func() { leaderResult <- owner.closePrepared(leaderCtx) }()
	select {
	case <-taskCanceled:
	case <-time.After(time.Second):
		t.Fatal("leader did not enter prepared cleanup")
	}

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelWaiter()
	if err := owner.closePrepared(waiterCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("active-attempt waiter error = %v, want deadline exceeded", err)
	}
	leaderErr := <-leaderResult
	var residual *runtime.TaskResidualError
	if !errors.As(leaderErr, &residual) || !errors.Is(leaderErr, context.DeadlineExceeded) {
		t.Fatalf("leader close = %v, residual = %#v", leaderErr, residual)
	}
	select {
	case <-owner.closeDone:
		t.Fatal("incomplete leader close signaled terminal closeDone")
	default:
	}

	close(release)
	if err := owner.closePrepared(context.Background()); err != nil {
		t.Fatalf("retry close = %v", err)
	}
}

func TestGenerationOwnerTerminalWaiterReturnPublishesCloseDone(t *testing.T) {
	fixture := newTestGenerationOwner(t, 52, true, false)
	owner := fixture.owner
	owner.activateDomains(ownerDomainHTTP)
	owner.deactivateDomains(ownerDomainHTTP)

	release := make(chan struct{})
	started := make(chan struct{})
	tasks := generationOwnerTaskRegistry(t, fixture.prepared)
	if err := tasks.Go(
		runtime.TaskSpec{Owner: "server-test/terminal-publication", Criticality: runtime.TaskPlugin},
		func(context.Context) error {
			close(started)
			<-release
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	<-started

	leaderResult := make(chan error, 1)
	go func() { leaderResult <- owner.closePrepared(context.Background()) }()
	var attempt *generationCloseAttempt
	for attempt == nil {
		owner.closeMu.Lock()
		attempt = owner.closeAttempt
		owner.closeMu.Unlock()
	}

	const waiters = 256
	violations := make(chan struct{}, waiters)
	var group sync.WaitGroup
	group.Add(waiters)
	for range waiters {
		go func() {
			defer group.Done()
			if err := waitGenerationCloseAttempt(context.Background(), attempt); err != nil {
				return
			}
			select {
			case <-owner.closeDone:
			default:
				violations <- struct{}{}
			}
		}()
	}
	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader close = %v", err)
	}
	group.Wait()
	close(violations)
	if len(violations) != 0 {
		t.Fatalf("%d terminal waiters returned before closeDone publication", len(violations))
	}
}

func generationOwnerTaskRegistry(t *testing.T, prepared *compiler.PreparedGeneration) *runtime.TaskRegistry {
	t.Helper()
	field := reflect.ValueOf(prepared).Elem().FieldByName("tasks")
	if !field.IsValid() || field.IsNil() {
		t.Fatal("prepared generation has no task registry")
	}
	return (*runtime.TaskRegistry)(unsafe.Pointer(field.Pointer()))
}

type generationOwnerFixture struct {
	owner        *generationOwner
	prepared     *compiler.PreparedGeneration
	registration *ownerTestRegistration
}

func newTestGenerationOwner(
	t *testing.T,
	revision uint64,
	httpEnabled bool,
	streamEnabled bool,
) generationOwnerFixture {
	t.Helper()
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &ownerTestMaterializer{digest: catalog.Digest()}
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
		materializer,
		compiler.WorkerRuntimeObservers{
			Cluster: proxy.NopClusterObserver{},
			Stream:  func(streamruntime.Result) {},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Errorf("WorkerCompilerFactory.Close() error = %v", err)
		}
	})

	resources := make([]generation.Resource, 0, 1)
	domains := make([]generation.Domain, 0, 2)
	if httpEnabled {
		domains = append(domains, generation.DomainHTTP)
	}
	if streamEnabled {
		domains = append(domains, generation.DomainStream)
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
	ticket := generation.ApplyTicket{
		DesiredRevision: desired.Revision(),
		DesiredDigest:   desired.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "generation-owner-test", Revision: "1"},
		RequiredDomains: slices.Clone(domains),
	}
	prepared, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if (prepared.HTTP() != nil) != httpEnabled || (prepared.Stream() != nil) != streamEnabled {
		t.Fatalf("prepared domains HTTP=%t stream=%t, want HTTP=%t stream=%t",
			prepared.HTTP() != nil, prepared.Stream() != nil, httpEnabled, streamEnabled)
	}
	if materializer.registration == nil {
		t.Fatal("materializer did not register prepared generation")
	}
	return generationOwnerFixture{
		owner:        newGenerationOwner(prepared),
		prepared:     prepared,
		registration: materializer.registration,
	}
}

func assertDrained(t *testing.T, owner *generationOwner) {
	t.Helper()
	select {
	case <-owner.drained:
	default:
		t.Fatal("owner is not drained")
	}
}

func assertNotDrained(t *testing.T, owner *generationOwner) {
	t.Helper()
	select {
	case <-owner.drained:
		t.Fatal("owner drained before its active domains and leases reached zero")
	default:
	}
}

func assertPanics(t *testing.T, operation func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("operation did not panic")
		}
	}()
	operation()
}

type ownerTestMaterializer struct {
	digest       [32]byte
	registration *ownerTestRegistration
}

func (materializer *ownerTestMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.registration = &ownerTestRegistration{id: secret.CandidateAttemptID(ticket, set)}
	return materializer.registration, nil
}

func (*ownerTestMaterializer) RegisterRecovery(
	context.Context,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	return nil, errors.New("unexpected recovery registration")
}

func (materializer *ownerTestMaterializer) DeclarationDigest() [32]byte {
	return materializer.digest
}

type ownerTestRegistration struct {
	id         secret.AttemptID
	closeCalls atomic.Int64
}

func (registration *ownerTestRegistration) AttemptID() secret.AttemptID {
	return registration.id
}

func (*ownerTestRegistration) Materialize(context.Context, secret.Scope, string) (secret.Value, error) {
	return secret.Value{}, secret.ErrCredentialUnavailable
}

func (registration *ownerTestRegistration) Close(context.Context) error {
	registration.closeCalls.Add(1)
	return nil
}
