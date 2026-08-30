package compiler

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type recordingAttemptMaterializer struct {
	digest         [32]byte
	trace          *[]string
	candidateSet   generation.PublicationSet
	registration   *recordingFactoryRegistration
	candidateCalls int
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

func (materializer *countingMaterializer) DeclarationDigest() [32]byte {
	return materializer.delegate.DeclarationDigest()
}

type mutatingTicketMaterializer struct {
	*recordingAttemptMaterializer
}

type mutatingSetMaterializer struct {
	*recordingAttemptMaterializer
}

type candidateResultMaterializer struct {
	*recordingAttemptMaterializer
	result *recordingFactoryRegistration
	err    error
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

func (materializer *mutatingSetMaterializer) RegisterCandidate(
	_ context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.candidateCalls++
	*materializer.trace = append(*materializer.trace, "register-candidate")
	registration := &recordingFactoryRegistration{
		id: secret.CandidateAttemptID(ticket, set), trace: materializer.trace,
	}
	materializer.registration = registration
	clear(set.Domains)
	return registration, nil
}

func (materializer *candidateResultMaterializer) RegisterCandidate(
	context.Context,
	generation.ApplyTicket,
	generation.PublicationSet,
) (secret.AttemptRegistration, error) {
	materializer.candidateCalls++
	*materializer.trace = append(*materializer.trace, "register-candidate")
	materializer.registration = materializer.result
	return materializer.result, materializer.err
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

type recordingConsumerPreparer struct {
	trace    *[]string
	err      error
	bindings *runtime.ConsumerBindings
}

func (preparer recordingConsumerPreparer) PrepareConsumers(
	context.Context,
	PreparationAttempt,
) (*runtime.ConsumerBindings, error) {
	*preparer.trace = append(*preparer.trace, "consumer")
	return preparer.bindings, preparer.err
}

func TestAttemptFactoryRegistersExactFinalSetWithoutPreparingConsumers(t *testing.T) {
	factory, materializer, consumers, trace := newRecordingAttemptFactory(t)
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
	if !reflect.DeepEqual(prepared.publication, want) {
		t.Fatalf("owned publication = %#v, want %#v", prepared.publication, want)
	}
	if got, wantTrace := *trace, []string{"register-candidate"}; !slices.Equal(got, wantTrace) {
		t.Fatalf("prepare trace = %v, want %v", got, wantTrace)
	}
	if prepared.attempt.AttemptID() != materializer.registration.id {
		t.Fatal("prepared attempt identity differs from registration")
	}
	bindings, err := consumers.PrepareConsumers(context.Background(), prepared.attempt)
	if err != nil {
		t.Fatal(err)
	}
	bindings.Close()

	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, wantTrace := *trace, []string{
		"register-candidate", "consumer", "registration-close",
	}; !slices.Equal(got, wantTrace) {
		t.Fatalf("trace = %v, want %v", got, wantTrace)
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

func TestAttemptFactoryOwnsPublicationBeforeRegistration(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	factory.materializer = &mutatingSetMaterializer{recordingAttemptMaterializer: materializer}
	desired := mustGenerationSnapshot(t, 491, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)

	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.publication.Domains) != 1 {
		t.Fatalf("owned publication domains = %d, want one", len(prepared.publication.Domains))
	}
	if _, ok := prepared.attempt.Candidate(generation.DomainHTTP); !ok {
		t.Fatal("attempt candidate was mutated through registration input")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptFactoryClosesRegistrationReturnedWithError(t *testing.T) {
	factory, materializer, _, trace := newRecordingAttemptFactory(t)
	registerErr := errors.New("candidate registration failed")
	closeErr := errors.New("candidate registration cleanup failed")
	registration := &recordingFactoryRegistration{trace: trace, closeErr: closeErr}
	factory.materializer = &candidateResultMaterializer{
		recordingAttemptMaterializer: materializer,
		result:                       registration,
		err:                          registerErr,
	}
	desired := mustGenerationSnapshot(t, 492, nil, nil)

	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, registerErr) || !errors.Is(err, closeErr) {
		t.Fatalf("registration failure = %#v/%v, want joined register/close errors", prepared, err)
	}
	if registration.closed != 1 || registration.closeCtx != nil {
		t.Fatalf("registration cleanup = %d/%v, want 1/<nil>", registration.closed, registration.closeCtx)
	}
}

func TestAttemptFactoryRejectsTypedNilRegistration(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	factory.materializer = &candidateResultMaterializer{recordingAttemptMaterializer: materializer}
	desired := mustGenerationSnapshot(t, 493, nil, nil)

	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("typed-nil registration = %#v/%v, want preparation failure", prepared, err)
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

func TestAttemptFactoryClosesRegistrationWhenCapabilityConstructionFails(t *testing.T) {
	factory, _, _, trace := newRecordingAttemptFactory(t)
	registration := &recordingFactoryRegistration{trace: trace}
	prepared, err := factory.prepareRegisteredAttempt(
		context.Background(),
		1,
		generation.PublicationSet{DesiredRevision: 1},
		nil,
		registration,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("capability failure = %#v/%v, want preparation failure", prepared, err)
	}
	if registration.closed != 1 {
		t.Fatalf("capability failure close calls = %d, want one", registration.closed)
	}
}

func TestAttemptFactoryClosesRegistrationWhenAttemptConstructionFails(t *testing.T) {
	factory, _, _, trace := newRecordingAttemptFactory(t)
	registration := &recordingFactoryRegistration{trace: trace}
	registration.id[0] = 1
	prepared, err := factory.prepareRegisteredAttempt(
		context.Background(),
		1,
		generation.PublicationSet{DesiredRevision: 1},
		[]factoryOccurrenceSpec{{}},
		registration,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("attempt construction failure = %#v/%v, want preparation failure", prepared, err)
	}
	if registration.closed != 1 {
		t.Fatalf("attempt construction failure close calls = %d, want one", registration.closed)
	}
}

func TestRegisteredAttemptCloseIsConcurrentAndIdempotent(t *testing.T) {
	factory, materializer, _, _ := newRecordingAttemptFactory(t)
	closeErr := errors.New("registration close failed")
	desired := mustGenerationSnapshot(t, 45, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	materializer.registration.closeErr = closeErr

	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			if err := prepared.Close(context.Background()); !errors.Is(err, closeErr) {
				t.Errorf("Close() error = %v, want %v", err, closeErr)
			}
		})
	}
	wait.Wait()
	if materializer.registration.closed != 1 {
		t.Fatalf("registration close calls = %d, want 1", materializer.registration.closed)
	}
}

func TestAttemptFactoryRejectsUnownedPluginTargetBeforeRegistration(t *testing.T) {
	factory, materializer, _, trace := newRecordingAttemptFactoryWithCompiler(
		t, newUnsupportedPluginTargetTestCompiler(t),
	)
	desired := mustGenerationSnapshot(t, 46, []generation.Resource{
		resourceValue(
			"routes", "r1",
			`{"id":"r1","plugins":{"echo":{"body":"$ENV://BODY"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if prepared != nil || !errors.Is(err, errAttemptPreparationFailed) {
		t.Fatalf("unowned plugin-target preparation = %#v/%v", prepared, err)
	}
	if materializer.candidateCalls != 0 || materializer.registration != nil {
		t.Fatalf("unowned plugin-target registration = %d/%#v, want zero/nil",
			materializer.candidateCalls, materializer.registration)
	}
	if len(*trace) != 0 {
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
						`{"id":"r1","plugins":{"echo":{"body":"$ENV://BODY"}}}`,
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
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			broker := &countingScopedBroker{}
			factory, materializer, _, trace := newScopedAttemptFactoryWithCompiler(
				t, newUnsupportedPluginTargetTestCompiler(t), broker,
			)
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
			if len(*trace) != 0 {
				t.Fatalf("unowned plugin-target trace = %v, want none", *trace)
			}
		})
	}
}

func TestAttemptFactoryRegistrationDoesNotPreparePluginSecrets(t *testing.T) {
	broker := &countingScopedBroker{resolveErr: errors.New("resolver exposed $ENV://FAIL")}
	factory, materializer, _, trace := newScopedAttemptFactory(t, broker)
	desired := mustGenerationSnapshot(t, 464, []generation.Resource{
		resourceValue(
			"routes", "r1",
			`{"id":"r1","plugins":{"basic-auth":{"password":"$ENV://FAIL"}}}`,
		),
	}, nil)
	prepared, err := factory.prepareCandidateAttempt(
		context.Background(), ticketForSnapshot(desired, generation.DomainHTTP), desired, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if materializer.candidateCalls != 1 || materializer.last == nil {
		t.Fatalf(
			"candidate registration = %d/%#v, want one registration",
			materializer.candidateCalls,
			materializer.last,
		)
	}
	if broker.candidateAuthorizations != 1 || broker.resolveCalls != 0 || broker.revokeCalls != 0 {
		t.Fatalf(
			"scoped broker calls = authorize:%d resolve:%d revoke:%d, want 1/0/0",
			broker.candidateAuthorizations, broker.resolveCalls, broker.revokeCalls,
		)
	}
	if len(*trace) != 0 {
		t.Fatalf("downstream preparation trace = %v, want none", *trace)
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.revokeCalls != 1 {
		t.Fatalf("close revocations = %d, want one", broker.revokeCalls)
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
) (*attemptFactory, *recordingAttemptMaterializer, *recordingConsumerPreparer, *[]string) {
	t.Helper()
	return newRecordingAttemptFactoryWithCompiler(t, newTestCompiler(t))
}

func newRecordingAttemptFactoryWithCompiler(
	t *testing.T,
	compiler *Compiler,
) (*attemptFactory, *recordingAttemptMaterializer, *recordingConsumerPreparer, *[]string) {
	t.Helper()
	trace := &[]string{}
	materializer := &recordingAttemptMaterializer{digest: compiler.schemas.catalog.Digest(), trace: trace}
	bindings, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumerPreparer := &recordingConsumerPreparer{trace: trace, bindings: bindings}
	factory, err := newAttemptFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer, consumerPreparer, trace
}

func newScopedAttemptFactory(
	t *testing.T,
	broker *countingScopedBroker,
) (*attemptFactory, *countingMaterializer, *recordingConsumerPreparer, *[]string) {
	t.Helper()
	return newScopedAttemptFactoryWithCompiler(t, newTestCompiler(t), broker)
}

func newScopedAttemptFactoryWithCompiler(
	t *testing.T,
	compiler *Compiler,
	broker *countingScopedBroker,
) (*attemptFactory, *countingMaterializer, *recordingConsumerPreparer, *[]string) {
	t.Helper()
	trace := &[]string{}
	materializer := &countingMaterializer{
		delegate: testutil.NewSecretMaterializer(broker, compiler.schemas.catalog),
	}
	bindings, err := runtime.NewConsumerBindings(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumerPreparer := &recordingConsumerPreparer{trace: trace, bindings: bindings}
	factory, err := newAttemptFactory(compiler, materializer)
	if err != nil {
		t.Fatal(err)
	}
	return factory, materializer, consumerPreparer, trace
}
