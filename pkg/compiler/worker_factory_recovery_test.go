package compiler

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

var _ func(
	*WorkerCompilerFactory,
	context.Context,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
	func(runtime.TaskFailure),
) (*PreparedGeneration, error) = (*WorkerCompilerFactory).PrepareRecovery

func TestWorkerCompilerFactoryPrepareRecoveryPreservesIndependentCommittedDomains(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	revisions, committed := workerRecoveryCommitted(t)
	expected := cloneRecoveryPublicationsForPreparation(committed)
	materializer.expectedRecovery = cloneRecoveryPublicationsForPreparation(expected)

	prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantPublication := workerRecoveryPublicationSet(revisions, expected)
	if got := prepared.PublicationSet(); !reflect.DeepEqual(got, wantPublication) {
		t.Fatalf("prepared publication = %#v, want %#v", got, wantPublication)
	}
	if !materializer.recoveryWasExact {
		t.Fatal("recovery materializer did not receive the exact verified committed map")
	}

	callerHTTP := committed[generation.DomainHTTP]
	callerHTTP.Closure[0] = generation.ResourceKey{Kind: "routes", ID: "caller-mutated"}
	callerHTTP.Decisions[0].Code = "caller-mutated"
	committed[generation.DomainHTTP] = callerHTTP
	delete(committed, generation.DomainStream)
	received := materializer.rawRecovery()
	httpReceived := received[generation.DomainHTTP]
	httpReceived.Closure[0] = generation.ResourceKey{Kind: "routes", ID: "recorder-mutated"}
	httpReceived.Decisions[0].Code = "recorder-mutated"
	received[generation.DomainHTTP] = httpReceived
	delete(received, generation.DomainStream)

	if got := prepared.PublicationSet(); !reflect.DeepEqual(got, wantPublication) {
		t.Fatalf("caller/recorder mutation changed prepared publication: %#v", got)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryTransfersBaseOwners(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	revisions, committed := workerRecoveryCommitted(t)
	materializer.expectedRecovery = cloneRecoveryPublicationsForPreparation(committed)
	var trace []string
	materializer.trace = func(stage string) { trace = append(trace, stage) }
	factory.checkpoint = func(stage string, _ workerFactoryCheckpointState) error {
		trace = append(trace, stage)
		return nil
	}

	prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := []string{
		"register-verified-recovery",
		"attempt-and-capability-ready",
		"create-task-registry",
		"prepare-consumers",
		"prepare-metadata",
		"bind-private-materializer-authority",
		"compile-http-snapshot",
		"compile-stream-snapshot",
		"transfer-prepared-generation",
	}
	if !slices.Equal(trace, wantTrace) {
		t.Fatalf("prepare trace = %v, want %v", trace, wantTrace)
	}
	wantID := secret.RecoveryAttemptID(revisions, cloneRecoveryPublicationsForPreparation(committed))
	if prepared.attempt.AttemptID() != wantID || prepared.attempt.Generation() != revisions.Desired {
		t.Fatalf("prepared attempt = %x/%d, want %x/%d",
			prepared.attempt.AttemptID(), prepared.attempt.Generation(), wantID, revisions.Desired)
	}
	if prepared.tasks == nil || prepared.consumers == nil || prepared.lookup.bindings == nil ||
		prepared.materializer != materializer || prepared.registry != factory.registry ||
		prepared.cleanup == nil || prepared.manifest != factory.compiler.manifest {
		t.Fatalf("prepared recovery did not receive exact base owners: %#v", prepared)
	}
	if prepared.HTTP() == nil || prepared.Stream() == nil ||
		prepared.Stream().Revision() != revisions.Stream ||
		!slices.Equal(prepared.Stream().Router().RouteIDs(), []string{"stream-live"}) {
		t.Fatal("recovery did not compile both committed protocol snapshots")
	}
	factory.liveMu.Lock()
	tracked := factory.live[wantID]
	factory.liveMu.Unlock()
	if tracked != prepared {
		t.Fatal("prepared recovery was not inserted as the live attempt owner")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.liveMu.Lock()
	live := len(factory.live)
	factory.liveMu.Unlock()
	if live != 0 || materializer.registrationsSnapshot()[0].closed != 1 {
		t.Fatalf("close left live=%d registration closes=%d", live, materializer.registrationsSnapshot()[0].closed)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryCompilesSystemRouteHTTPSnapshot(t *testing.T) {
	factory, _ := newWorkerRecoveryTestFactory(t)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 5}
	snapshot := mustGenerationSnapshot(t, 5, []generation.Resource{
		resourceValue("routes", "plain", `{"id":"plain"}`),
	}, nil)
	committed := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, snapshot),
	}
	prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if factory.registry.Len() != 3 || prepared.ConsumerLookup() == nil ||
		prepared.PublicationSet().DesiredRevision != revisions.Desired || prepared.HTTP() == nil ||
		prepared.HTTP().Revision() != revisions.HTTP {
		t.Fatalf("system-route recovery returned unusable owner/resource count %d", factory.registry.Len())
	}
	if prepared.HTTP().TLSConfig() != nil {
		t.Fatal("system-route recovery exposed TLS config while frontend TLS is disabled")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryRejectsMismatchesBeforeOwners(t *testing.T) {
	baseRevisions, baseCommitted := workerRecoveryCommitted(t)
	tests := map[string]func(*generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration){
		"revision": func(revisions *generation.RevisionSet, _ map[generation.Domain]generation.PublishedGeneration) {
			revisions.HTTP--
		},
		"domain": func(_ *generation.RevisionSet, committed map[generation.Domain]generation.PublishedGeneration) {
			delete(committed, generation.DomainStream)
		},
		"artifact": func(_ *generation.RevisionSet, committed map[generation.Domain]generation.PublishedGeneration) {
			value := committed[generation.DomainHTTP]
			value.Artifact.Digest[0]++
			committed[generation.DomainHTTP] = value
		},
		"closure": func(_ *generation.RevisionSet, committed map[generation.Domain]generation.PublishedGeneration) {
			value := committed[generation.DomainHTTP]
			value.Closure = value.Closure[1:]
			committed[generation.DomainHTTP] = value
		},
		"decision": func(_ *generation.RevisionSet, committed map[generation.Domain]generation.PublishedGeneration) {
			value := committed[generation.DomainHTTP]
			for index := range value.Decisions {
				if value.Decisions[index].Disposition == generation.DispositionPublished {
					value.Decisions[index].Disposition = generation.DispositionDeleted
					break
				}
			}
			committed[generation.DomainHTTP] = value
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			factory, materializer := newWorkerRecoveryTestFactory(t)
			materializer.panicCandidate = true
			factory.checkpoint = func(string, workerFactoryCheckpointState) error {
				t.Fatal("mismatched recovery reached task/consumer transfer")
				return nil
			}
			revisions := baseRevisions
			committed := cloneRecoveryPublicationsForPreparation(baseCommitted)
			mutate(&revisions, committed)
			prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
			if prepared != nil || err == nil {
				t.Fatalf("mismatched recovery = %#v/%v, want nil/error", prepared, err)
			}
			candidateCalls, recoveryCalls := materializer.callCounts()
			if candidateCalls != 0 || recoveryCalls != 0 || factory.registry.Len() != 0 {
				t.Fatalf("mismatch side effects candidate/recovery/resources = %d/%d/%d",
					candidateCalls, recoveryCalls, factory.registry.Len())
			}
			factory.liveMu.Lock()
			live := len(factory.live)
			factory.liveMu.Unlock()
			if live != 0 {
				t.Fatalf("mismatch inserted %d live generations", live)
			}
		})
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryRedactsRegistrationAndPartialCloseErrors(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	registrationErr := &workerTestSecretError{text: "plaintext-recovery-registration"}
	closeErr := &workerTestSecretError{text: "plaintext-recovery-close"}
	materializer.recoveryErr = registrationErr
	materializer.closeErr = closeErr
	revisions, committed := workerRecoveryCommitted(t)
	prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if prepared != nil {
		t.Fatalf("partial registration returned owner %#v", prepared)
	}
	assertWorkerErrorRedacted(t, err, registrationErr, closeErr)
	registrations := materializer.registrationsSnapshot()
	if len(registrations) != 1 || registrations[0].closed != 1 {
		t.Fatalf("partial registration closes = %#v", registrations)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryDoesNotAliasCandidateResources(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	const routeID = "shared-request-id"
	desired := mustGenerationSnapshot(t, 9, []generation.Resource{
		resourceValue("routes", routeID, `{"id":"shared-request-id","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	candidate, err := factory.PrepareGeneration(context.Background(), ticket, desired, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	recoverySnapshot := mustGenerationSnapshot(t, 5, []generation.Resource{
		resourceValue("routes", routeID, `{"id":"shared-request-id","plugins":{"request-id":{}}}`),
	}, nil)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 5}
	committed := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, recoverySnapshot),
	}
	recovery, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if err != nil {
		t.Fatal(err)
	}

	candidateBinding := materializeWorkerRecoveryRequestID(t, candidate, routeID)
	recoveryBinding := materializeWorkerRecoveryRequestID(t, recovery, routeID)
	candidateID := secret.CandidateAttemptID(ticket, materializer.candidatePublication())
	recoveryID := secret.RecoveryAttemptID(revisions, cloneRecoveryPublicationsForPreparation(committed))
	if candidateBinding.InstanceKey.Attempt != candidateID ||
		recoveryBinding.InstanceKey.Attempt != recoveryID || candidateID == recoveryID {
		t.Fatalf("candidate/recovery instance attempts = %x/%x, want %x/%x",
			candidateBinding.InstanceKey.Attempt, recoveryBinding.InstanceKey.Attempt, candidateID, recoveryID)
	}
	if got := factory.registry.Len(); got != 4 {
		t.Fatalf("candidate/recovery compiled resources = %d, want 4 isolated resources", got)
	}
	if err := recovery.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := factory.registry.Len(); got != 0 {
		t.Fatalf("shared registry resources after close = %d, want 0", got)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryRejectsLiveAttemptCollision(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	revisions, committed := workerRecoveryCommitted(t)
	start := make(chan struct{})
	results := make(chan *PreparedGeneration, 2)
	errorsOut := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Go(func() {
			<-start
			prepared, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
			results <- prepared
			errorsOut <- err
		})
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsOut)
	var winner *PreparedGeneration
	successes, failures := 0, 0
	for prepared := range results {
		if prepared != nil {
			successes++
			winner = prepared
		}
	}
	for err := range errorsOut {
		if err != nil {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("collision successes/failures = %d/%d, want 1/1", successes, failures)
	}
	factory.liveMu.Lock()
	tracked := factory.live[winner.attempt.AttemptID()]
	factory.liveMu.Unlock()
	if tracked != winner {
		t.Fatal("duplicate recovery replaced the original live owner")
	}
	registrations := materializer.registrationsSnapshot()
	if len(registrations) != 2 || registrations[0].closed+registrations[1].closed != 1 {
		t.Fatalf("collision registration cleanup = %#v", registrations)
	}
	if err := winner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry, err := factory.PrepareRecovery(context.Background(), revisions, committed, nil)
	if err != nil {
		t.Fatalf("retry after detach failed: %v", err)
	}
	if err := retry.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCompilerFactoryPrepareRecoveryRejectsInvalidPreflight(t *testing.T) {
	factory, materializer := newWorkerRecoveryTestFactory(t)
	revisions, committed := workerRecoveryCommitted(t)
	var nilContext context.Context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, call := range map[string]func() (*PreparedGeneration, error){
		"nil factory": func() (*PreparedGeneration, error) {
			return (*WorkerCompilerFactory)(nil).PrepareRecovery(context.Background(), revisions, committed, nil)
		},
		"nil context": func() (*PreparedGeneration, error) {
			return factory.PrepareRecovery(nilContext, revisions, committed, nil)
		},
		"canceled context": func() (*PreparedGeneration, error) {
			return factory.PrepareRecovery(ctx, revisions, committed, nil)
		},
		"closed factory": func() (*PreparedGeneration, error) {
			factory.gate.Lock()
			factory.closed = true
			factory.gate.Unlock()
			return factory.PrepareRecovery(context.Background(), revisions, committed, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			prepared, err := call()
			if prepared != nil || err == nil {
				t.Fatalf("preflight = %#v/%v, want nil/error", prepared, err)
			}
		})
	}
	if candidateCalls, recoveryCalls := materializer.callCounts(); candidateCalls != 0 || recoveryCalls != 0 {
		t.Fatalf("preflight registered candidate/recovery = %d/%d", candidateCalls, recoveryCalls)
	}
}

type workerRecoveryTestMaterializer struct {
	mu               sync.Mutex
	digest           [32]byte
	candidateCalls   int
	recoveryCalls    int
	candidateSet     generation.PublicationSet
	recoveryReceived map[generation.Domain]generation.PublishedGeneration
	expectedRecovery map[generation.Domain]generation.PublishedGeneration
	recoveryWasExact bool
	registrations    []*workerTestRegistration
	trace            func(string)
	panicCandidate   bool
	recoveryErr      error
	closeErr         error
}

func (materializer *workerRecoveryTestMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	if materializer.panicCandidate {
		panic("recovery path called candidate registration")
	}
	materializer.candidateCalls++
	materializer.candidateSet = clonePublicationSetForPreparation(set)
	registration := &workerTestRegistration{id: secret.CandidateAttemptID(ticket, set)}
	materializer.registrations = append(materializer.registrations, registration)
	return registration, nil
}

func (materializer *workerRecoveryTestMaterializer) RegisterRecovery(
	_ context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	materializer.recoveryCalls++
	materializer.recoveryReceived = published
	materializer.recoveryWasExact = materializer.expectedRecovery == nil ||
		reflect.DeepEqual(published, materializer.expectedRecovery)
	if materializer.trace != nil && materializer.recoveryWasExact {
		materializer.trace("register-verified-recovery")
	}
	registration := &workerTestRegistration{
		id: secret.RecoveryAttemptID(revisions, published), closeErr: materializer.closeErr,
	}
	materializer.registrations = append(materializer.registrations, registration)
	return registration, materializer.recoveryErr
}

func (materializer *workerRecoveryTestMaterializer) DeclarationDigest() [32]byte {
	if materializer == nil {
		return [32]byte{}
	}
	return materializer.digest
}

func (materializer *workerRecoveryTestMaterializer) rawRecovery() map[generation.Domain]generation.PublishedGeneration {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	return materializer.recoveryReceived
}

func (materializer *workerRecoveryTestMaterializer) candidatePublication() generation.PublicationSet {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	return clonePublicationSetForPreparation(materializer.candidateSet)
}

func (materializer *workerRecoveryTestMaterializer) callCounts() (int, int) {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	return materializer.candidateCalls, materializer.recoveryCalls
}

func (materializer *workerRecoveryTestMaterializer) registrationsSnapshot() []*workerTestRegistration {
	materializer.mu.Lock()
	defer materializer.mu.Unlock()
	return slices.Clone(materializer.registrations)
}

func newWorkerRecoveryTestFactory(
	t *testing.T,
) (*WorkerCompilerFactory, *workerRecoveryTestMaterializer) {
	t.Helper()
	manifest := mustManifest(t)
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	materializer := &workerRecoveryTestMaterializer{digest: compiler.schemas.catalog.Digest()}
	factory, err := NewWorkerCompilerFactory(manifest, workerTestEffective(manifest), materializer)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer
}

func workerRecoveryCommitted(
	t *testing.T,
) (generation.RevisionSet, map[generation.Domain]generation.PublishedGeneration) {
	t.Helper()
	httpSnapshot := mustGenerationSnapshot(t, 5, []generation.Resource{
		resourceValue("routes", "http-live", `{"id":"http-live"}`),
	}, []generation.Tombstone{{
		Key: generation.ResourceKey{Kind: "routes", ID: "http-deleted"}, Revision: 4,
	}})
	streamSnapshot := mustGenerationSnapshot(t, 6, []generation.Resource{
		resourceValue("stream_routes", "stream-live", `{
			"id":"stream-live",
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
		}`),
	}, []generation.Tombstone{{
		Key: generation.ResourceKey{Kind: "stream_routes", ID: "stream-deleted"}, Revision: 5,
	}})
	return generation.RevisionSet{Desired: 9, HTTP: 5, Stream: 6},
		map[generation.Domain]generation.PublishedGeneration{
			generation.DomainHTTP:   publishedForDomain(generation.DomainHTTP, httpSnapshot),
			generation.DomainStream: publishedForDomain(generation.DomainStream, streamSnapshot),
		}
}

func workerRecoveryPublicationSet(
	revisions generation.RevisionSet,
	committed map[generation.Domain]generation.PublishedGeneration,
) generation.PublicationSet {
	domains := make(map[generation.Domain]generation.PublicationCandidate, len(committed))
	for domain, published := range committed {
		domains[domain] = generation.PublicationCandidate(published)
	}
	return generation.PublicationSet{DesiredRevision: revisions.Desired, Domains: domains}
}

func materializeWorkerRecoveryRequestID(
	t *testing.T,
	prepared *PreparedGeneration,
	routeID string,
) plugin.Binding {
	t.Helper()
	var occurrence FactoryOccurrence
	for _, candidate := range prepared.attempt.Occurrences(capability.SecretPluginConfig) {
		if candidate.Factory() == "request-id" && candidate.Resource() == (generation.ResourceKey{
			Kind: "routes", ID: routeID,
		}) {
			occurrence = candidate
			break
		}
	}
	if occurrence.Factory() == "" {
		t.Fatal("prepared attempt has no exact request-id occurrence")
	}
	scope, provenance, ok := effectivePluginSourceIdentity(occurrence.Resource())
	if !ok {
		t.Fatalf("request-id occurrence has unsupported resource %#v", occurrence.Resource())
	}
	bindings, err := prepared.materializeEffectiveBindings(context.Background(), []effectiveBindingSpec{{
		domain:         generation.DomainHTTP,
		executionOwner: occurrence.Resource(),
		source: effectiveBindingSource{
			kind: effectiveBindingPluginConfig, resource: occurrence.Resource(),
			source: capability.SecretPluginConfig, occurrence: occurrence,
		},
		factory:    "request-id",
		config:     resource.PluginConfig(map[string]any{}),
		scope:      scope,
		provenance: provenance,
		resourceContext: effectiveBindingResourceContext{
			kind:  effectiveBindingContextHTTP,
			route: resource.Route{ID: routeID},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 {
		t.Fatalf("materialized bindings = %d, want 1", len(bindings))
	}
	return bindings[0]
}
