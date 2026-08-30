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

var errGenerationSecretPreparationFailed = errors.New("generation secret preparation failed")

type generationFactory struct {
	compiler     *Compiler
	materializer secret.Materializer
}

func newGenerationFactory(
	compiler *Compiler,
	materializer secret.Materializer,
) (*generationFactory, error) {
	if compiler == nil || compiler.manifest == nil || compiler.schemas == nil ||
		isNilInterface(materializer) {
		return nil, fmt.Errorf("%w: generation factory dependencies are required", ErrInvalidInput)
	}
	if materializer.DeclarationDigest() != compiler.schemas.catalog.Digest() {
		return nil, fmt.Errorf("%w: secret declaration catalog mismatch", ErrInvalidInput)
	}
	return &generationFactory{compiler: compiler, materializer: materializer}, nil
}

func (factory *generationFactory) prepareGenerationSecrets(
	ctx context.Context,
	ticket generation.ApplyTicket,
	desired generation.Snapshot,
	previous map[generation.Domain]generation.PublishedGeneration,
) (*preparedSecretGeneration, error) {
	if err := validateGenerationFactoryContext(factory, ctx); err != nil {
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
	if !publicationCanPrepare(set) {
		return nil, errGenerationSecretPreparationFailed
	}
	// This is deliberately adjacent to the first side effect. Everything above
	// is pure and a forged post-refinement set cannot reach materialization.
	if err := generation.ValidatePublicationSet(ownedTicket, set); err != nil {
		return nil, err
	}
	ownedSet := clonePublicationSetForPreparation(set)
	materialization, err := factory.materializer.PrepareGeneration(
		ctx, clonePublicationSetForPreparation(ownedSet),
	)
	if err != nil {
		if isNilInterface(materialization) {
			return nil, err
		}
		return nil, errors.Join(err, materialization.Close(context.WithoutCancel(ctx)))
	}
	if isNilInterface(materialization) {
		return nil, errGenerationSecretPreparationFailed
	}
	secrets := materialization.Secrets()
	if !secrets.Valid() || secrets.Generation() != ticket.DesiredRevision {
		return nil, errors.Join(ErrInvalidInput, materialization.Close(context.WithoutCancel(ctx)))
	}
	preparation, err := newPreparationGeneration(
		ticket.DesiredRevision, clonePreparationCandidates(ownedSet.Domains), secrets, occurrences,
	)
	if err != nil {
		return nil, errors.Join(errGenerationSecretPreparationFailed, materialization.Close(context.WithoutCancel(ctx)))
	}
	return &preparedSecretGeneration{
		preparation:     preparation,
		publication:     clonePublicationSetForPreparation(ownedSet),
		materialization: materialization,
	}, nil
}

func validateGenerationFactoryContext(factory *generationFactory, ctx context.Context) error {
	if factory == nil || factory.compiler == nil || isNilInterface(factory.materializer) || ctx == nil {
		return fmt.Errorf("%w: generation factory is not initialized", ErrInvalidInput)
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

type preparedSecretGeneration struct {
	preparation     PreparationGeneration
	publication     generation.PublicationSet
	materialization secret.GenerationMaterialization
	closeOnce       sync.Once
	closeErr        error
}

func (prepared *preparedSecretGeneration) Close(ctx context.Context) error {
	if prepared == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	prepared.closeOnce.Do(func() {
		if !isNilInterface(prepared.materialization) {
			prepared.closeErr = prepared.materialization.Close(cleanupCtx)
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
