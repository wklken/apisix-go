package compiler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errAttemptPreparationFailed = errors.New("generation attempt preparation failed")

type attemptFactory struct {
	compiler     *Compiler
	materializer secret.Materializer
	metadata     MetadataPreparer
	consumers    ConsumerPreparer
	plugins      PluginPreparer
}

func newAttemptFactory(
	compiler *Compiler,
	materializer secret.Materializer,
	metadata MetadataPreparer,
	consumers ConsumerPreparer,
	plugins PluginPreparer,
) (*attemptFactory, error) {
	if compiler == nil || compiler.manifest == nil || compiler.schemas == nil ||
		isNilInterface(materializer) || isNilInterface(metadata) ||
		isNilInterface(consumers) || isNilInterface(plugins) {
		return nil, fmt.Errorf("%w: attempt factory dependencies are required", ErrInvalidInput)
	}
	if materializer.DeclarationDigest() != compiler.schemas.catalog.Digest() {
		return nil, fmt.Errorf("%w: secret declaration catalog mismatch", ErrInvalidInput)
	}
	return &attemptFactory{
		compiler: compiler, materializer: materializer,
		metadata: metadata, consumers: consumers, plugins: plugins,
	}, nil
}

func (factory *attemptFactory) prepareCandidateAttempt(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (*registeredAttempt, error) {
	if err := validateAttemptFactoryContext(factory, ctx); err != nil {
		return nil, err
	}
	ownedTicket := cloneApplyTicketForPreparation(ticket)
	set, err := factory.compiler.PreparePublication(ctx, ownedTicket, desired, previous)
	if err != nil {
		return nil, err
	}
	occurrences, err := finalFactoryOccurrences(ctx, ownedTicket, set, factory.compiler.schemas)
	if err != nil {
		return nil, err
	}
	if err := validateScopedSecretSupport(occurrences, factory.compiler.schemas.catalog); err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, err)
	}
	if !publicationCanPrepare(set) {
		return nil, errAttemptPreparationFailed
	}
	// This is deliberately adjacent to the first side effect. Everything above
	// is pure and a forged post-refinement set cannot reach registration.
	if err := generation.ValidatePublicationSet(ownedTicket, set); err != nil {
		return nil, err
	}
	ownedSet := clonePublicationSetForPreparation(set)
	registration, err := factory.materializer.RegisterCandidate(
		ctx, cloneApplyTicketForPreparation(ownedTicket), clonePublicationSetForPreparation(ownedSet),
	)
	if err != nil {
		if isNilInterface(registration) {
			return nil, err
		}
		return nil, errors.Join(err, registration.Close(context.WithoutCancel(ctx)))
	}
	if isNilInterface(registration) {
		return nil, errAttemptPreparationFailed
	}
	if registration.AttemptID() != secret.CandidateAttemptID(ownedTicket, ownedSet) {
		cleanupErr := registration.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(
			fmt.Errorf("%w: candidate registration identity mismatch", ErrInvalidInput), cleanupErr,
		)
	}
	return factory.prepareRegisteredAttempt(
		ctx, ticket.DesiredRevision, ownedSet.Domains, occurrences, registration,
	)
}

func (factory *attemptFactory) prepareRecoveryAttempt(
	ctx context.Context,
	revisions generation.RevisionSet,
	committed map[generation.Domain]generation.PublishedGeneration,
) (*registeredAttempt, error) {
	if err := validateAttemptFactoryContext(factory, ctx); err != nil {
		return nil, err
	}
	verified, err := validateRecovery(
		ctx, revisions, committed, factory.compiler.manifest, factory.compiler.schemas,
	)
	if err != nil {
		return nil, err
	}
	candidates := make(map[generation.Domain]generation.PublicationCandidate, len(verified))
	for domain, published := range verified {
		candidates[domain] = generation.PublicationCandidate(published)
	}
	occurrences, err := factoryOccurrencesFromCandidates(ctx, candidates, factory.compiler.schemas)
	if err != nil {
		return nil, err
	}
	if err := validateScopedSecretSupport(occurrences, factory.compiler.schemas.catalog); err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, err)
	}
	registration, err := factory.materializer.RegisterRecovery(
		ctx, revisions, cloneRecoveryPublicationsForPreparation(verified),
	)
	if err != nil {
		if isNilInterface(registration) {
			return nil, err
		}
		return nil, errors.Join(err, registration.Close(context.WithoutCancel(ctx)))
	}
	if isNilInterface(registration) {
		return nil, errAttemptPreparationFailed
	}
	if registration.AttemptID() != secret.RecoveryAttemptID(revisions, verified) {
		cleanupErr := registration.Close(context.WithoutCancel(ctx))
		return nil, errors.Join(
			fmt.Errorf("%w: recovery registration identity mismatch", ErrInvalidInput), cleanupErr,
		)
	}
	return factory.prepareRegisteredAttempt(ctx, revisions.Desired, candidates, occurrences, registration)
}

func (factory *attemptFactory) prepareRegisteredAttempt(
	ctx context.Context,
	generationNumber uint64,
	candidates map[generation.Domain]generation.PublicationCandidate,
	occurrences []factoryOccurrenceSpec,
	registration secret.AttemptRegistration,
) (*registeredAttempt, error) {
	capabilityValue, err := secret.NewGenerationCapability(registration, generationNumber)
	if err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, registration.Close(context.WithoutCancel(ctx)))
	}
	attempt, err := newPreparationAttempt(generationNumber, candidates, capabilityValue, occurrences)
	if err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, registration.Close(context.WithoutCancel(ctx)))
	}
	prepared := &registeredAttempt{attempt: attempt, registration: registration}

	if err := prepareCompilerDiscardSecrets(ctx, attempt, factory.compiler.schemas.catalog); err != nil {
		return nil, prepared.fail(ctx, err)
	}
	prepared.metadata, err = factory.metadata.PrepareMetadata(ctx, attempt)
	if err != nil {
		return nil, prepared.fail(ctx, err)
	}
	prepared.consumers, err = factory.consumers.PrepareConsumers(ctx, attempt, prepared.metadata)
	if err != nil {
		return nil, prepared.fail(ctx, err)
	}
	if prepared.consumers == nil {
		return nil, prepared.fail(ctx, errAttemptPreparationFailed)
	}
	prepared.plugins, err = factory.plugins.PreparePlugins(
		ctx, attempt, prepared.metadata, consumerLookupView{bindings: prepared.consumers},
	)
	if err != nil {
		return nil, prepared.fail(ctx, err)
	}
	if isNilInterface(prepared.plugins) {
		return nil, prepared.fail(ctx, errAttemptPreparationFailed)
	}
	return prepared, nil
}

func validateAttemptFactoryContext(factory *attemptFactory, ctx context.Context) error {
	if factory == nil || factory.compiler == nil || isNilInterface(factory.materializer) || ctx == nil {
		return fmt.Errorf("%w: attempt factory is not initialized", ErrInvalidInput)
	}
	return ctx.Err()
}

func publicationCanPrepare(set generation.PublicationSet) bool {
	decisions := 0
	for _, candidate := range set.Domains {
		for _, decision := range candidate.Decisions {
			decisions++
			if decision.Disposition == generation.DispositionPublished ||
				decision.Disposition == generation.DispositionLastGood ||
				decision.Disposition == generation.DispositionDeleted {
				return true
			}
		}
	}
	return decisions == 0
}

type consumerLookupView struct{ bindings *runtime.ConsumerBindings }

func (view consumerLookupView) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	return view.bindings.ConsumerByPluginKey(plugin, key)
}

func (view consumerLookupView) ConsumerByID(id string) (resource.Consumer, bool) {
	return view.bindings.ConsumerByID(id)
}

func (view consumerLookupView) ConsumerGroupByID(id string) (resource.ConsumerGroup, bool) {
	return view.bindings.ConsumerGroupByID(id)
}

type registeredAttempt struct {
	attempt      PreparationAttempt
	metadata     runtime.MetadataView
	consumers    *runtime.ConsumerBindings
	plugins      PreparedPlugins
	registration secret.AttemptRegistration
	closeOnce    sync.Once
	closeErr     error
}

func (prepared *registeredAttempt) fail(ctx context.Context, cause error) error {
	return errors.Join(errAttemptPreparationFailed, cause, prepared.Close(context.WithoutCancel(ctx)))
}

func (prepared *registeredAttempt) Close(ctx context.Context) error {
	if prepared == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	prepared.closeOnce.Do(func() {
		var cleanupErrors []error
		if !isNilInterface(prepared.plugins) {
			cleanupErrors = append(cleanupErrors, prepared.plugins.Close(cleanupCtx))
		}
		if prepared.consumers != nil {
			prepared.consumers.Close()
		}
		if prepared.registration != nil {
			cleanupErrors = append(cleanupErrors, prepared.registration.Close(cleanupCtx))
		}
		prepared.metadata = runtime.MetadataView{}
		prepared.closeErr = errors.Join(cleanupErrors...)
	})
	return prepared.closeErr
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneRecoveryPublicationsForPreparation(
	published map[generation.Domain]generation.PublishedGeneration,
) map[generation.Domain]generation.PublishedGeneration {
	clone := make(map[generation.Domain]generation.PublishedGeneration, len(published))
	for domain, value := range published {
		clone[domain] = clonePublishedGenerationForRecovery(value)
	}
	return clone
}

func cloneApplyTicketForPreparation(ticket generation.ApplyTicket) generation.ApplyTicket {
	ticket.RequiredDomains = slices.Clone(ticket.RequiredDomains)
	return ticket
}

var _ base.ConsumerLookup = consumerLookupView{}
