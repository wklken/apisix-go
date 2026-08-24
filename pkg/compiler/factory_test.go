package compiler

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

type recordingAttemptMaterializer struct {
	digest         [32]byte
	trace          *[]string
	candidateSet   generation.PublicationSet
	recoveryState  map[generation.Domain]generation.PublishedGeneration
	registration   *recordingFactoryRegistration
	candidateCalls int
	recoveryCalls  int
}

type countingScopedBroker struct {
	candidateAuthorizations int
	recoveryAuthorizations  int
	resolveCalls            int
	revokeCalls             int
	resolveErr              error
}

func (broker *countingScopedBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	broker.candidateAuthorizations++
	return nil
}

func (broker *countingScopedBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	broker.recoveryAuthorizations++
	return nil
}

func (broker *countingScopedBroker) ResolveScoped(
	context.Context,
	secret.Scope,
	string,
) (string, error) {
	broker.resolveCalls++
	if broker.resolveErr != nil {
		return "", broker.resolveErr
	}
	return "resolved", nil
}

func (broker *countingScopedBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	broker.revokeCalls++
	return nil
}

type countingMaterializer struct {
	delegate       secret.Materializer
	candidateCalls int
	recoveryCalls  int
	last           secret.AttemptRegistration
}

func (materializer *countingMaterializer) RegisterCandidate(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.candidateCalls++
	registration, err := materializer.delegate.RegisterCandidate(ctx, ticket, set)
	materializer.last = registration
	return registration, err
}

func (materializer *countingMaterializer) RegisterRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	materializer.recoveryCalls++
	registration, err := materializer.delegate.RegisterRecovery(ctx, revisions, published)
	materializer.last = registration
	return registration, err
}

func (materializer *countingMaterializer) DeclarationDigest() [32]byte {
	return materializer.delegate.DeclarationDigest()
}

type mutatingTicketMaterializer struct {
	*recordingAttemptMaterializer
}

func (materializer *mutatingTicketMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.candidateCalls++
	*materializer.trace = append(*materializer.trace, "register-candidate")
	ticket.RequiredDomains[0] = generation.DomainStream
	registration := &recordingFactoryRegistration{
		id: secret.CandidateAttemptID(ticket, set), trace: materializer.trace,
	}
	materializer.registration = registration
	return registration, nil
}

func (materializer *recordingAttemptMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.candidateCalls++
	*materializer.trace = append(*materializer.trace, "register-candidate")
	materializer.candidateSet = clonePublicationSetForPreparation(set)
	registration := &recordingFactoryRegistration{
		id: secret.CandidateAttemptID(ticket, set), trace: materializer.trace,
	}
	materializer.registration = registration
	return registration, nil
}

func (materializer *recordingAttemptMaterializer) RegisterRecovery(
	_ context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (secret.AttemptRegistration, error) {
	materializer.recoveryCalls++
	*materializer.trace = append(*materializer.trace, "register-recovery")
	materializer.recoveryState = cloneRecoveryMapForFactoryTest(published)
	registration := &recordingFactoryRegistration{
		id: secret.RecoveryAttemptID(revisions, published), trace: materializer.trace,
	}
	materializer.registration = registration
	return registration, nil
}

func (materializer *recordingAttemptMaterializer) DeclarationDigest() [32]byte {
	return materializer.digest
}

type recordingFactoryRegistration struct {
	id       secret.AttemptID
	trace    *[]string
	closeErr error
	closeCtx error
	closed   int
}

func (registration *recordingFactoryRegistration) AttemptID() secret.AttemptID {
	return registration.id
}

func (*recordingFactoryRegistration) Materialize(
	context.Context, secret.Scope, string,
) (secret.Value, error) {
	return secret.Value{}, nil
}

func (registration *recordingFactoryRegistration) Close(ctx context.Context) error {
	registration.closed++
	registration.closeCtx = ctx.Err()
	*registration.trace = append(*registration.trace, "registration-close")
	return registration.closeErr
}

type recordingMetadataPreparer struct {
	trace *[]string
	err   error
}

func (preparer recordingMetadataPreparer) PrepareMetadata(
	context.Context,
	PreparationAttempt,
) (runtime.MetadataView, error) {
	*preparer.trace = append(*preparer.trace, "metadata")
	return runtime.MetadataView{}, preparer.err
}

type recordingConsumerPreparer struct {
	trace    *[]string
	err      error
	bindings *runtime.ConsumerBindings
}

func (preparer recordingConsumerPreparer) PrepareConsumers(
	context.Context,
	PreparationAttempt,
	runtime.MetadataView,
) (*runtime.ConsumerBindings, error) {
	*preparer.trace = append(*preparer.trace, "consumer")
	return preparer.bindings, preparer.err
}

type recordingPluginPreparer struct {
	trace  *[]string
	err    error
	owner  *recordingPreparedPlugins
	lookup base.ConsumerLookup
}

func (preparer *recordingPluginPreparer) PreparePlugins(
	_ context.Context,
	_ PreparationAttempt,
	_ runtime.MetadataView,
	lookup base.ConsumerLookup,
) (PreparedPlugins, error) {
	*preparer.trace = append(*preparer.trace, "plugin")
	preparer.lookup = lookup
	return preparer.owner, preparer.err
}

type recordingPreparedPlugins struct {
	trace    *[]string
	closeErr error
	closeCtx error
	closed   int
}

func (plugins *recordingPreparedPlugins) Close(ctx context.Context) error {
	plugins.closed++
	plugins.closeCtx = ctx.Err()
	*plugins.trace = append(*plugins.trace, "plugin-close")
	return plugins.closeErr
}

func TestAttemptFactoryRegistersExactFinalSetBeforeHooks(t *testing.T) {
	factory, materializer, pluginPreparer, trace := newRecordingAttemptFactory(t)
	desired := mustGenerationSnapshot(t, 41, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	want, err := factory.compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	prepared, err := factory.prepareCandidateAttempt(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(materializer.candidateSet, want) {
		t.Fatalf("registered set = %#v, want %#v", materializer.candidateSet, want)
	}
	if got, wantTrace := *trace, []string{
		"register-candidate",
		"metadata",
		"consumer",
		"plugin",
	}; !slices.Equal(
		got,
		wantTrace,
	) {
		t.Fatalf("prepare trace = %v, want %v", got, wantTrace)
	}
	if _, exposesClose := pluginPreparer.lookup.(*runtime.ConsumerBindings); exposesClose {
		t.Fatal("plugin hook received ConsumerBindings close authority")
	}
	if prepared.attempt.AttemptID() != materializer.registration.id {
		t.Fatal("prepared attempt identity differs from registration")
	}

	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, wantTrace := (*trace)[4:], []string{"plugin-close", "registration-close"}; !slices.Equal(got, wantTrace) {
		t.Fatalf("close trace = %v, want %v", got, wantTrace)
	}
}

func TestAttemptFactoryRejectsInvalidDesiredBeforeRegistration(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	desired := mustGenerationSnapshot(t, 42, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{"algorithm":"invalid"}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	if _, err := factory.prepareCandidateAttempt(
		context.Background(),
		ticket,
		desired,
		nil,
	); !errors.Is(
		err,
		errAttemptPreparationFailed,
	) {
		t.Fatalf("invalid desired error = %v, want preparation failure", err)
	}
	if materializer.candidateCalls != 0 {
		t.Fatalf("candidate registrations = %d, want zero for wholly fail-closed set", materializer.candidateCalls)
	}
}

func TestAttemptFactoryOwnsTicketBeforeRegistration(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	mutating := &mutatingTicketMaterializer{recordingAttemptMaterializer: materializer}
	factory.materializer = mutating
	desired := mustGenerationSnapshot(t, 49, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)

	prepared, err := factory.prepareCandidateAttempt(context.Background(), ticket, desired, nil)
	if prepared != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mutated ticket result = %#v/%v, want identity rejection", prepared, err)
	}
	if !slices.Equal(ticket.RequiredDomains, []generation.Domain{generation.DomainHTTP}) {
		t.Fatalf("caller ticket domains mutated to %v", ticket.RequiredDomains)
	}
	if materializer.registration == nil || materializer.registration.closed != 1 {
		t.Fatalf("mutated ticket registration cleanup = %#v", materializer.registration)
	}
}

func TestAttemptFactoryHookFailureCleansPartialOwnersWithUncanceledContext(t *testing.T) {
	factory, materializer, pluginPreparer, trace := newRecordingAttemptFactory(t)
	pluginErr := errors.New("plugin hook failed")
	closeErr := errors.New("plugin close failed")
	pluginPreparer.err = pluginErr
	pluginPreparer.owner.closeErr = closeErr
	desired := mustGenerationSnapshot(t, 43, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Cancellation before preparation is rejected without registration.
	if _, err := factory.prepareCandidateAttempt(
		ctx,
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Fatalf("canceled prepare error = %v", err)
	}
	if materializer.candidateCalls != 0 {
		t.Fatal("canceled preparation registered an attempt")
	}

	pluginPreparer.owner.closeCtx = context.Canceled
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, pluginErr) || !errors.Is(err, closeErr) {
		t.Fatalf("partial hook result = %#v/%v", prepared, err)
	}
	if pluginPreparer.owner.closed != 1 || materializer.registration.closed != 1 ||
		pluginPreparer.owner.closeCtx != nil || materializer.registration.closeCtx != nil {
		t.Fatalf("cleanup owners/contexts plugin=%d/%v registration=%d/%v",
			pluginPreparer.owner.closed, pluginPreparer.owner.closeCtx,
			materializer.registration.closed, materializer.registration.closeCtx)
	}
	if got, want := (*trace)[len(*trace)-2:], []string{"plugin-close", "registration-close"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup trace = %v, want suffix %v", *trace, want)
	}
}

func TestAttemptFactoryRejectsRegistrationWithWrongAttemptID(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	materializer.registration = &recordingFactoryRegistration{}
	materializerOverride := &wrongIDMaterializer{recordingAttemptMaterializer: materializer}
	factory.materializer = materializerOverride
	desired := mustGenerationSnapshot(t, 44, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	if _, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong registration id error = %v, want ErrInvalidInput", err)
	}
	if materializer.registration.closed != 1 {
		t.Fatalf("wrong registration close calls = %d", materializer.registration.closed)
	}
}

func TestRegisteredAttemptCloseIsConcurrentAndIdempotent(t *testing.T) {
	factory, materializer, pluginPreparer, _ := newRecordingAttemptFactory(t)
	desired := mustGenerationSnapshot(t, 45, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			if err := prepared.Close(context.Background()); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})
	}
	wait.Wait()
	if pluginPreparer.owner.closed != 1 || materializer.registration.closed != 1 {
		t.Fatalf("close calls plugin=%d registration=%d, want 1/1",
			pluginPreparer.owner.closed, materializer.registration.closed)
	}
}

func TestAttemptFactoryRejectsUnownedPluginTargetBeforeRegistration(t *testing.T) {
	factory, materializer, pluginPreparer, trace := newRecordingAttemptFactory(t)
	desired := mustGenerationSnapshot(t, 46, []generation.Resource{
		resourceValue(
			"routes", "r1",
			`{"id":"r1","plugins":{"response-rewrite":{"body":"$ENV://BODY"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("unowned plugin-target preparation = %#v/%v", prepared, err)
	}
	if pluginPreparer.lookup != nil {
		t.Fatal("unowned plugin-target factory reached plugin hook")
	}
	if materializer.candidateCalls != 0 || materializer.registration != nil {
		t.Fatalf("unowned plugin-target registration = %d/%#v, want zero/nil",
			materializer.candidateCalls, materializer.registration)
	}
	if len(*trace) != 0 || slices.Contains(*trace, "plugin") {
		t.Fatalf("unowned plugin-target preparation trace = %v, want empty", *trace)
	}
}

func TestAttemptFactoryRejectsUnownedPluginTargetBeforeScopedRegistration(t *testing.T) {
	tests := map[string]struct {
		prepare func(*attemptFactory) error
		check   func(*countingMaterializer)
	}{
		"candidate": {
			prepare: func(factory *attemptFactory) error {
				desired := mustGenerationSnapshot(t, 461, []generation.Resource{
					resourceValue(
						"routes", "r1",
						`{"id":"r1","plugins":{"response-rewrite":{"body":"$ENV://BODY"}}}`,
					),
				}, nil)
				_, err := factory.prepareCandidateAttempt(
					context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
				)
				return err
			},
			check: func(materializer *countingMaterializer) {
				if materializer.candidateCalls != 0 {
					t.Fatalf("candidate registrations = %d, want zero", materializer.candidateCalls)
				}
			},
		},
		"recovery": {
			prepare: func(factory *attemptFactory) error {
				snapshot := mustGenerationSnapshot(t, 462, []generation.Resource{
					resourceValue(
						"routes", "r1",
						`{"id":"r1","plugins":{"response-rewrite":{"body":"$ENV://BODY"}}}`,
					),
				}, nil)
				_, err := factory.prepareRecoveryAttempt(
					context.Background(),
					generation.RevisionSet{Desired: 463, HTTP: snapshot.Revision()},
					map[generation.Domain]generation.PublishedGeneration{
						generation.DomainHTTP: publishedForDomain(generation.DomainHTTP, snapshot),
					},
				)
				return err
			},
			check: func(materializer *countingMaterializer) {
				if materializer.recoveryCalls != 0 {
					t.Fatalf("recovery registrations = %d, want zero", materializer.recoveryCalls)
				}
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			broker := &countingScopedBroker{}
			factory, materializer, pluginPreparer, trace := newScopedAttemptFactory(t, broker)
			if err := test.prepare(factory); !errors.Is(err, errAttemptPreparationFailed) {
				t.Fatalf("unowned plugin-target error = %v, want preparation failure", err)
			}
			test.check(materializer)
			if broker.candidateAuthorizations != 0 || broker.recoveryAuthorizations != 0 ||
				broker.resolveCalls != 0 || broker.revokeCalls != 0 {
				t.Fatalf(
					"scoped broker calls = candidate-authorize:%d recovery-authorize:%d resolve:%d revoke:%d, want all zero",
					broker.candidateAuthorizations,
					broker.recoveryAuthorizations,
					broker.resolveCalls,
					broker.revokeCalls,
				)
			}
			if pluginPreparer.lookup != nil || len(*trace) != 0 {
				t.Fatalf("unowned plugin-target hooks = lookup:%#v trace:%v, want none", pluginPreparer.lookup, *trace)
			}
		})
	}
}

func TestAttemptFactoryCompilerDiscardFailureCleansRegistrationBeforeHooks(t *testing.T) {
	broker := &countingScopedBroker{resolveErr: errors.New("resolver exposed $ENV://FAIL")}
	factory, materializer, pluginPreparer, trace := newScopedAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 464, []generation.Resource{
		resourceValue(
			"routes", "r1",
			`{"id":"r1","plugins":{"basic-auth":{"password":"$ENV://FAIL"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("compiler-discard failure = %#v/%v, want preparation failure", prepared, err)
	}
	if materializer.candidateCalls != 1 || materializer.last == nil {
		t.Fatalf(
			"candidate registration = %d/%#v, want one registration",
			materializer.candidateCalls,
			materializer.last,
		)
	}
	if broker.candidateAuthorizations != 1 || broker.resolveCalls != 1 || broker.revokeCalls != 1 {
		t.Fatalf(
			"scoped broker calls = authorize:%d resolve:%d revoke:%d, want 1/1/1",
			broker.candidateAuthorizations, broker.resolveCalls, broker.revokeCalls,
		)
	}
	if pluginPreparer.lookup != nil || len(*trace) != 0 {
		t.Fatalf("compiler-discard failure hooks = lookup:%#v trace:%v, want none", pluginPreparer.lookup, *trace)
	}
}

func TestAttemptFactoryAllowsValidEmptyAndDeleteOnlyPublications(t *testing.T) {
	tests := map[string]generation.Snapshot{
		"valid empty": mustGenerationSnapshot(t, 47, nil, nil),
		"delete only": mustGenerationSnapshot(t, 48, nil, []generation.Tombstone{{
			Key: generation.ResourceKey{Kind: "routes", ID: "r1"}, Revision: 47,
		}}),
	}
	for name, desired := range tests {
		t.Run(name, func(t *testing.T) {
			factory, materializer, _, _ := newRecordingAttemptFactory(t)
			prepared, err := factory.prepareCandidateAttempt(
				context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if materializer.candidateCalls != 1 {
				t.Fatalf("candidate registrations = %d, want 1", materializer.candidateCalls)
			}
			if err := prepared.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type wrongIDMaterializer struct{ *recordingAttemptMaterializer }

func (materializer *wrongIDMaterializer) RegisterCandidate(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	registration, err := materializer.recordingAttemptMaterializer.RegisterCandidate(ctx, ticket, set)
	if err == nil {
		materializer.registration.id[0]++
	}
	return registration, err
}

func newRecordingAttemptFactory(
	t *testing.T,
) (*attemptFactory, *recordingAttemptMaterializer, *recordingPluginPreparer, *[]string) {
	t.Helper()
	compiler := newTestCompiler(t)
	trace := &[]string{}
	materializer := &recordingAttemptMaterializer{digest: compiler.schemas.catalog.Digest(), trace: trace}
	bindings, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pluginPreparer := &recordingPluginPreparer{
		trace: trace,
		owner: &recordingPreparedPlugins{trace: trace},
	}
	factory, err := newAttemptFactory(
		compiler,
		materializer,
		recordingMetadataPreparer{trace: trace},
		recordingConsumerPreparer{trace: trace, bindings: bindings},
		pluginPreparer,
	)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer, pluginPreparer, trace
}

func newScopedAttemptFactory(
	t *testing.T,
	broker *countingScopedBroker,
) (*attemptFactory, *countingMaterializer, *recordingPluginPreparer, *[]string) {
	t.Helper()
	compiler := newTestCompiler(t)
	trace := &[]string{}
	materializer := &countingMaterializer{
		delegate: secret.NewScopedMaterializer(broker, compiler.schemas.catalog),
	}
	bindings, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pluginPreparer := &recordingPluginPreparer{
		trace: trace,
		owner: &recordingPreparedPlugins{trace: trace},
	}
	factory, err := newAttemptFactory(
		compiler,
		materializer,
		recordingMetadataPreparer{trace: trace},
		recordingConsumerPreparer{trace: trace, bindings: bindings},
		pluginPreparer,
	)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer, pluginPreparer, trace
}

func cloneRecoveryMapForFactoryTest(
	published map[generation.Domain]generation.PublishedGeneration,
) map[generation.Domain]generation.PublishedGeneration {
	clone := make(map[generation.Domain]generation.PublishedGeneration, len(published))
	for domain, value := range published {
		clone[domain] = clonePublishedGenerationForRecovery(value)
	}
	return clone
}
