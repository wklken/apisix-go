package compiler

import (
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
)

type attemptAuthority struct{ marker byte }

// FactoryOccurrence binds a published factory instance to one attempt-owned
// domain and resource. Values can be copied but cannot be forged for another
// PreparationAttempt because the authority token is package-private.
type FactoryOccurrence struct {
	authority *attemptAuthority
	domain    generation.Domain
	resource  generation.ResourceKey
	source    capability.SecretDeclarationSource
	factory   string
}

func (occurrence FactoryOccurrence) Domain() generation.Domain { return occurrence.domain }

func (occurrence FactoryOccurrence) Resource() generation.ResourceKey { return occurrence.resource }

func (occurrence FactoryOccurrence) Source() capability.SecretDeclarationSource {
	return occurrence.source
}

func (occurrence FactoryOccurrence) Factory() string { return occurrence.factory }

// PreparationAttempt exposes only attempt-bound access. Registration and
// consumer lifecycle ownership remain with the factory transaction.
type PreparationAttempt struct {
	authority   *attemptAuthority
	generation  uint64
	capability  secret.GenerationCapability
	candidates  map[generation.Domain]generation.PublicationCandidate
	occurrences []FactoryOccurrence
}

func newPreparationAttempt(
	generationNumber uint64,
	candidates map[generation.Domain]generation.PublicationCandidate,
	capabilityValue secret.GenerationCapability,
	specs []factoryOccurrenceSpec,
) (PreparationAttempt, error) {
	if generationNumber == 0 || !capabilityValue.Valid() || capabilityValue.Generation() != generationNumber {
		return PreparationAttempt{}, fmt.Errorf("%w: preparation capability is invalid", ErrInvalidInput)
	}
	authority := &attemptAuthority{marker: 1}
	attempt := PreparationAttempt{
		authority:  authority,
		generation: generationNumber,
		capability: capabilityValue,
		candidates: clonePreparationCandidates(candidates),
	}
	attempt.occurrences = make([]FactoryOccurrence, 0, len(specs))
	for _, spec := range specs {
		if !validOccurrenceSpec(spec) {
			return PreparationAttempt{}, fmt.Errorf("%w: factory occurrence is invalid", ErrInvalidInput)
		}
		attempt.occurrences = append(attempt.occurrences, FactoryOccurrence{
			authority: authority,
			domain:    spec.domain, resource: spec.resource, source: spec.source, factory: spec.factory,
		})
	}
	return attempt, nil
}

func (attempt PreparationAttempt) Generation() uint64 { return attempt.generation }

func (attempt PreparationAttempt) AttemptID() secret.AttemptID { return attempt.capability.AttemptID() }

func (attempt PreparationAttempt) Candidate(
	domain generation.Domain,
) (generation.PublicationCandidate, bool) {
	candidate, exists := attempt.candidates[domain]
	if !exists {
		return generation.PublicationCandidate{}, false
	}
	return clonePublicationCandidateForPreparation(candidate), true
}

func (attempt PreparationAttempt) Occurrences(
	source capability.SecretDeclarationSource,
) []FactoryOccurrence {
	result := make([]FactoryOccurrence, 0)
	for _, occurrence := range attempt.occurrences {
		if occurrence.source == source {
			result = append(result, occurrence)
		}
	}
	return result
}

func (attempt PreparationAttempt) MaterializeSecret(
	ctx context.Context,
	occurrence FactoryOccurrence,
	field string,
	raw string,
) (secret.Value, error) {
	if ctx == nil || field == "" || !attempt.owns(occurrence) {
		return secret.Value{}, fmt.Errorf("%w: secret occurrence authority is invalid", ErrInvalidInput)
	}
	scope := secret.Scope{
		Generation: attempt.generation,
		Attempt:    attempt.AttemptID(),
		Domain:     occurrence.domain,
		Plugin:     occurrence.factory,
		Resource:   occurrence.resource,
		Source:     occurrence.source,
		Field:      field,
	}
	return attempt.capability.Materialize(ctx, scope, raw)
}

func (attempt PreparationAttempt) materializeCompositeSecret(
	ctx context.Context,
	occurrence compositeChildOccurrence,
	field string,
	raw string,
) (secret.Value, error) {
	if ctx == nil || field == "" || occurrence.factory == "" ||
		!attempt.owns(occurrence.outer) ||
		occurrence.outer.source != capability.SecretPluginConfig {
		return secret.Value{}, fmt.Errorf("%w: composite secret occurrence authority is invalid", ErrInvalidInput)
	}
	return attempt.capability.Materialize(ctx, secret.Scope{
		Generation: attempt.generation,
		Attempt:    attempt.AttemptID(),
		Domain:     occurrence.outer.domain,
		Plugin:     occurrence.factory,
		Resource:   occurrence.outer.resource,
		Source:     occurrence.outer.source,
		Field:      field,
	}, raw)
}

func (attempt PreparationAttempt) PrepareScopedPluginSecrets(
	ctx context.Context,
	occurrence FactoryOccurrence,
	bound plugin.FactoryInstance,
) error {
	instance := bound.Plugin()
	if ctx == nil || isNilInterface(instance) || !attempt.owns(occurrence) ||
		occurrence.source != capability.SecretPluginConfig {
		return fmt.Errorf("%w: scoped plugin occurrence authority is invalid", ErrInvalidInput)
	}
	if bound.Factory() != occurrence.factory {
		return fmt.Errorf("%w: scoped plugin factory identity is invalid", ErrInvalidInput)
	}
	return plugin.MaterializeScopedPluginSecrets(ctx, secret.Scope{
		Generation: attempt.generation,
		Attempt:    attempt.AttemptID(),
		Domain:     occurrence.domain,
		Plugin:     occurrence.factory,
		Resource:   occurrence.resource,
		Source:     occurrence.source,
	}, attempt.capability, instance)
}

func (attempt PreparationAttempt) owns(occurrence FactoryOccurrence) bool {
	return attempt.authority != nil && occurrence.authority == attempt.authority &&
		validOccurrence(occurrence) && attempt.capability.Valid() &&
		attempt.capability.Generation() == attempt.generation
}

func validOccurrence(occurrence FactoryOccurrence) bool {
	return occurrence.authority != nil && validOccurrenceSpec(factoryOccurrenceSpec{
		domain: occurrence.domain, resource: occurrence.resource,
		source: occurrence.source, factory: occurrence.factory,
	})
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
	PrepareMetadata(context.Context, PreparationAttempt) (runtime.MetadataView, error)
}

type ConsumerPreparer interface {
	PrepareConsumers(context.Context, PreparationAttempt, runtime.MetadataView) (*runtime.ConsumerBindings, error)
}

type PluginPreparer interface {
	PreparePlugins(
		context.Context,
		PreparationAttempt,
		runtime.MetadataView,
		base.ConsumerLookup,
	) (PreparedPlugins, error)
}

type PreparedPlugins interface {
	Close(context.Context) error
}
