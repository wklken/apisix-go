package compiler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
)

var errAttemptPreparationFailed = errors.New("generation attempt preparation failed")

type attemptFactory struct {
	compiler     *Compiler
	materializer secret.Materializer
}

func newAttemptFactory(
	compiler *Compiler,
	materializer secret.Materializer,
) (*attemptFactory, error) {
	if compiler == nil || compiler.manifest == nil || compiler.schemas == nil ||
		isNilInterface(materializer) {
		return nil, fmt.Errorf("%w: attempt factory dependencies are required", ErrInvalidInput)
	}
	if materializer.DeclarationDigest() != compiler.schemas.catalog.Digest() {
		return nil, fmt.Errorf("%w: secret declaration catalog mismatch", ErrInvalidInput)
	}
	return &attemptFactory{compiler: compiler, materializer: materializer}, nil
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
	compositeChildren, err := compositeChildOccurrenceSpecsFromCandidates(
		ctx, set.Domains, occurrences,
	)
	if err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, err)
	}
	if err := validateCompositeScopedSecretSupport(
		compositeChildren, factory.compiler.schemas.catalog,
	); err != nil {
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
		ctx, ticket.DesiredRevision, ownedSet, occurrences, registration,
	)
}

func (factory *attemptFactory) prepareRegisteredAttempt(
	ctx context.Context,
	generationNumber uint64,
	publication generation.PublicationSet,
	occurrences []factoryOccurrenceSpec,
	registration secret.AttemptRegistration,
) (*registeredAttempt, error) {
	capabilityValue, err := secret.NewGenerationCapability(registration, generationNumber)
	if err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, registration.Close(context.WithoutCancel(ctx)))
	}
	attempt, err := newPreparationAttempt(
		generationNumber,
		clonePreparationCandidates(publication.Domains),
		capabilityValue,
		occurrences,
	)
	if err != nil {
		return nil, errors.Join(errAttemptPreparationFailed, registration.Close(context.WithoutCancel(ctx)))
	}
	return &registeredAttempt{
		attempt:      attempt,
		publication:  clonePublicationSetForPreparation(publication),
		registration: registration,
	}, nil
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

type registeredAttempt struct {
	attempt      PreparationAttempt
	publication  generation.PublicationSet
	registration secret.AttemptRegistration
	closeOnce    sync.Once
	closeErr     error
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
		if !isNilInterface(prepared.registration) {
			prepared.closeErr = prepared.registration.Close(cleanupCtx)
		}
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

func cloneApplyTicketForPreparation(ticket generation.ApplyTicket) generation.ApplyTicket {
	ticket.RequiredDomains = slices.Clone(ticket.RequiredDomains)
	return ticket
}
