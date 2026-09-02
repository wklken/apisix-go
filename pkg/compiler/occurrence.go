package compiler

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

type factoryOccurrenceSpec struct {
	domain   generation.Domain
	resource generation.ResourceKey
	source   capability.SecretDeclarationSource
	factory  string
}

func finalFactoryOccurrences(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
	schemas *schemaSet,
) ([]factoryOccurrenceSpec, error) {
	if ctx == nil || schemas == nil {
		return nil, fmt.Errorf("%w: occurrence boundary is not initialized", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := generation.ValidatePublicationSet(ticket, set); err != nil {
		return nil, err
	}
	return factoryOccurrencesFromCandidates(ctx, set.Domains, schemas)
}

func factoryOccurrencesFromCandidates(
	ctx context.Context,
	candidates map[generation.Domain]generation.PublicationCandidate,
	schemas *schemaSet,
) ([]factoryOccurrenceSpec, error) {
	if ctx == nil || schemas == nil {
		return nil, fmt.Errorf("%w: occurrence boundary is not initialized", ErrInvalidInput)
	}
	occurrences := make([]factoryOccurrenceSpec, 0)
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		candidate, exists := candidates[domain]
		if !exists {
			continue
		}
		input, issues, err := normalizeContext(ctx, candidate.Snapshot)
		if err != nil {
			return nil, err
		}
		if len(issues) != 0 {
			return nil, generation.ErrIntegrity
		}
		for _, key := range input.keys() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			resource, published := input.resources[key]
			if !published {
				continue
			}
			switch key.Kind {
			case "plugins":
				continue
			case "plugin_metadata":
				if err := appendFactoryOccurrence(
					&occurrences, schemas, domain, key, capability.SecretPluginMetadata, key.ID,
				); err != nil {
					return nil, err
				}
			case "consumers":
				for _, factory := range sortedFactories(resource.view.plugins) {
					if pluginConfigDisabled(resource.view.plugins[factory]) {
						continue
					}
					if err := appendFactoryOccurrence(
						&occurrences, schemas, domain, key, capability.SecretConsumerConfig, factory,
					); err != nil {
						return nil, err
					}
				}
			default:
				if !regularPluginResourceKind(key.Kind) {
					continue
				}
				for _, factory := range sortedFactories(resource.view.plugins) {
					if pluginConfigDisabled(resource.view.plugins[factory]) {
						continue
					}
					entry, exists := schemas.factories[factory]
					if !exists {
						return nil, generation.ErrIntegrity
					}
					if !slices.Contains(entry.domains, domain) {
						continue
					}
					occurrences = append(occurrences, factoryOccurrenceSpec{
						domain: domain, resource: key, source: capability.SecretPluginConfig, factory: factory,
					})
				}
			}
		}
	}
	return occurrences, nil
}

func appendFactoryOccurrence(
	occurrences *[]factoryOccurrenceSpec,
	schemas *schemaSet,
	domain generation.Domain,
	resource generation.ResourceKey,
	source capability.SecretDeclarationSource,
	factory string,
) error {
	entry, exists := schemas.factories[factory]
	if !exists || !slices.Contains(entry.domains, domain) {
		return generation.ErrIntegrity
	}
	*occurrences = append(*occurrences, factoryOccurrenceSpec{
		domain: domain, resource: resource, source: source, factory: factory,
	})
	return nil
}

func sortedFactories(configs map[string]any) []string {
	factories := make([]string, 0, len(configs))
	for factory := range configs {
		factories = append(factories, factory)
	}
	sort.Strings(factories)
	return factories
}

func clonePublicationSetForPreparation(set generation.PublicationSet) generation.PublicationSet {
	clone := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		clone.Domains[domain] = clonePublicationCandidateForPreparation(candidate)
	}
	return clone
}

func clonePublicationCandidateForPreparation(
	candidate generation.PublicationCandidate,
) generation.PublicationCandidate {
	return generation.PublicationCandidate{
		Artifact:  candidate.Artifact,
		Snapshot:  candidate.Snapshot.Clone(),
		Closure:   slices.Clone(candidate.Closure),
		Decisions: slices.Clone(candidate.Decisions),
	}
}
