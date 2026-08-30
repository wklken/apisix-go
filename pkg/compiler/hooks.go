package compiler

import (
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

// FactoryOccurrence binds a published factory instance to one generation,
// domain, and resource. Its fields stay compiler-private.
type FactoryOccurrence struct {
	owner    secret.GenerationSecrets
	domain   generation.Domain
	resource generation.ResourceKey
	source   capability.SecretDeclarationSource
	factory  string
}

func (occurrence FactoryOccurrence) Domain() generation.Domain { return occurrence.domain }

func (occurrence FactoryOccurrence) Resource() generation.ResourceKey { return occurrence.resource }

func (occurrence FactoryOccurrence) Source() capability.SecretDeclarationSource {
	return occurrence.source
}

func (occurrence FactoryOccurrence) Factory() string { return occurrence.factory }

// PreparationGeneration owns the immutable publication candidates, exact
// plugin occurrences, and generation-local secret view used during compile.
type PreparationGeneration struct {
	generation    uint64
	secrets       secret.GenerationSecrets
	candidates    map[generation.Domain]generation.PublicationCandidate
	occurrences   []FactoryOccurrence
	occurrenceSet map[factoryOccurrenceSpec]struct{}
}

func newPreparationGeneration(
	generationNumber uint64,
	candidates map[generation.Domain]generation.PublicationCandidate,
	secrets secret.GenerationSecrets,
	specs []factoryOccurrenceSpec,
) (PreparationGeneration, error) {
	if generationNumber == 0 || !secrets.Valid() || secrets.Generation() != generationNumber {
		return PreparationGeneration{}, fmt.Errorf("%w: preparation capability is invalid", ErrInvalidInput)
	}
	preparation := PreparationGeneration{
		generation: generationNumber,
		secrets:    secrets,
		candidates: clonePreparationCandidates(candidates),
	}
	preparation.occurrences = make([]FactoryOccurrence, 0, len(specs))
	preparation.occurrenceSet = make(map[factoryOccurrenceSpec]struct{}, len(specs))
	for _, spec := range specs {
		if !validOccurrenceSpec(spec) {
			return PreparationGeneration{}, fmt.Errorf("%w: factory occurrence is invalid", ErrInvalidInput)
		}
		preparation.occurrences = append(preparation.occurrences, FactoryOccurrence{
			owner: secrets, domain: spec.domain, resource: spec.resource,
			source: spec.source, factory: spec.factory,
		})
		preparation.occurrenceSet[spec] = struct{}{}
	}
	return preparation, nil
}

func (preparation PreparationGeneration) Generation() uint64 { return preparation.generation }

func (preparation PreparationGeneration) Secrets() secret.GenerationSecrets {
	return preparation.secrets
}

func (preparation PreparationGeneration) Candidate(
	domain generation.Domain,
) (generation.PublicationCandidate, bool) {
	candidate, exists := preparation.candidates[domain]
	if !exists {
		return generation.PublicationCandidate{}, false
	}
	return clonePublicationCandidateForPreparation(candidate), true
}

func (preparation PreparationGeneration) Occurrences(
	source capability.SecretDeclarationSource,
) []FactoryOccurrence {
	result := make([]FactoryOccurrence, 0)
	for _, occurrence := range preparation.occurrences {
		if occurrence.source == source {
			result = append(result, occurrence)
		}
	}
	return result
}

func (preparation PreparationGeneration) MaterializeSecret(
	ctx context.Context,
	occurrence FactoryOccurrence,
	field string,
	raw string,
) (secret.Value, error) {
	if ctx == nil || field == "" || !preparation.owns(occurrence) {
		return secret.Value{}, fmt.Errorf("%w: secret occurrence is invalid", ErrInvalidInput)
	}
	scope := secret.Scope{
		Generation: preparation.generation,
		Domain:     occurrence.domain,
		Plugin:     occurrence.factory,
		Resource:   occurrence.resource,
		Source:     occurrence.source,
		Field:      field,
	}
	return preparation.secrets.Materialize(ctx, scope, raw)
}

func (preparation PreparationGeneration) PrepareScopedPluginSecrets(
	ctx context.Context,
	occurrence FactoryOccurrence,
	bound plugin.FactoryInstance,
) error {
	instance := bound.Plugin()
	if ctx == nil || isNilInterface(instance) || !preparation.owns(occurrence) ||
		(occurrence.source != capability.SecretPluginConfig &&
			occurrence.source != capability.SecretConsumerConfig) {
		return fmt.Errorf("%w: scoped plugin occurrence is invalid", ErrInvalidInput)
	}
	if bound.Factory() != occurrence.factory {
		return fmt.Errorf("%w: scoped plugin factory identity is invalid", ErrInvalidInput)
	}
	return plugin.MaterializeScopedPluginSecrets(ctx, secret.Scope{
		Generation: preparation.generation,
		Domain:     occurrence.domain,
		Plugin:     occurrence.factory,
		Resource:   occurrence.resource,
		Source:     occurrence.source,
	}, preparation.secrets, instance)
}

func (preparation PreparationGeneration) owns(occurrence FactoryOccurrence) bool {
	_, declared := preparation.occurrenceSet[occurrence.spec()]
	return validOccurrence(occurrence) && preparation.secrets.Valid() &&
		preparation.secrets.Generation() == preparation.generation &&
		occurrence.owner.SameGeneration(preparation.secrets) && declared
}

func validOccurrence(occurrence FactoryOccurrence) bool {
	return validOccurrenceSpec(occurrence.spec())
}

func (occurrence FactoryOccurrence) spec() factoryOccurrenceSpec {
	return factoryOccurrenceSpec{
		domain: occurrence.domain, resource: occurrence.resource,
		source: occurrence.source, factory: occurrence.factory,
	}
}

func validOccurrenceSpec(spec factoryOccurrenceSpec) bool {
	if (spec.domain != generation.DomainHTTP && spec.domain != generation.DomainStream) ||
		spec.resource.Kind == "" || spec.resource.ID == "" || spec.factory == "" ||
		!slices.Contains(generation.DomainsForResourceKind(spec.resource.Kind), spec.domain) {
		return false
	}
	switch spec.source {
	case capability.SecretPluginMetadata:
		return spec.resource.Kind == "plugin_metadata" && spec.resource.ID == spec.factory
	case capability.SecretConsumerConfig:
		return spec.resource.Kind == "consumers"
	case capability.SecretPluginConfig:
		return regularPluginResourceKind(spec.resource.Kind) && spec.resource.Kind != "consumers"
	default:
		return false
	}
}

func clonePreparationCandidates(
	candidates map[generation.Domain]generation.PublicationCandidate,
) map[generation.Domain]generation.PublicationCandidate {
	clone := make(map[generation.Domain]generation.PublicationCandidate, len(candidates))
	for domain, candidate := range candidates {
		clone[domain] = clonePublicationCandidateForPreparation(candidate)
	}
	return clone
}

type MetadataPreparer interface {
	PrepareMetadata(context.Context, PreparationGeneration) (runtime.MetadataView, error)
}

type ConsumerPreparer interface {
	PrepareConsumers(context.Context, PreparationGeneration) (*runtime.ConsumerBindings, error)
}
