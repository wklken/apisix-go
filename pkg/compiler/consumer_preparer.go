package compiler

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/wklken/apisix-go/pkg/capability"
	consumerregistry "github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/util"
)

var errConsumerPreparationFailed = fmt.Errorf(
	"%w: consumer binding preparation failed",
	ErrInvalidInput,
)

type consumerBindingPreparer struct {
	catalog *capability.SecretDeclarationCatalog
}

type consumerOccurrenceKey struct {
	resource generation.ResourceKey
	factory  string
}

func newConsumerBindingPreparer(
	catalog *capability.SecretDeclarationCatalog,
) (*consumerBindingPreparer, error) {
	if catalog == nil {
		return nil, errConsumerPreparationFailed
	}
	return &consumerBindingPreparer{catalog: catalog}, nil
}

func (preparer *consumerBindingPreparer) PrepareConsumers(
	ctx context.Context,
	attempt PreparationAttempt,
) (*runtime.ConsumerBindings, error) {
	if ctx == nil || preparer == nil || preparer.catalog == nil ||
		attempt.authority == nil || !attempt.capability.Valid() ||
		attempt.Generation() == 0 || attempt.Generation() != attempt.capability.Generation() {
		return nil, errConsumerPreparationFailed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	occurrences, err := indexConsumerOccurrences(attempt)
	if err != nil {
		return nil, errConsumerPreparationFailed
	}
	candidate, exists := attempt.Candidate(generation.DomainHTTP)
	if !exists {
		if len(occurrences) != 0 {
			return nil, errConsumerPreparationFailed
		}
		return runtime.NewConsumerBindings(nil, nil, nil)
	}
	if err := generation.ValidatePublicationCandidate(
		generation.DomainHTTP,
		candidate.Artifact.Revision,
		candidate,
	); err != nil {
		return nil, errConsumerPreparationFailed
	}

	input, issues, err := normalizeContext(ctx, candidate.Snapshot)
	if err != nil {
		return nil, consumerPreparationError(ctx)
	}
	if len(issues) != 0 {
		return nil, errConsumerPreparationFailed
	}
	if err := validateConsumerOccurrenceSet(input, occurrences); err != nil {
		return nil, errConsumerPreparationFailed
	}

	consumers := make([]runtime.ConsumerRecord, 0)
	groups := make([]runtime.ConsumerGroupRecord, 0)
	credentials := make([]runtime.ConsumerCredentialBinding, 0)
	consumed := make(map[consumerOccurrenceKey]struct{}, len(occurrences))
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		normalized := input.resources[key]
		switch key.Kind {
		case "consumers":
			consumerRecord, bindings, used, prepareErr := preparer.prepareConsumer(
				ctx,
				attempt,
				normalized,
				occurrences,
			)
			if prepareErr != nil {
				return nil, consumerPreparationError(ctx)
			}
			consumers = append(consumers, consumerRecord)
			credentials = append(credentials, bindings...)
			for _, occurrence := range used {
				consumed[occurrence] = struct{}{}
			}
		case "consumer_groups":
			var group resource.ConsumerGroup
			if err := util.Parse(normalized.document, &group); err != nil {
				return nil, errConsumerPreparationFailed
			}
			group.ConfigDigest = sha256.Sum256(normalized.raw)
			groups = append(groups, runtime.ConsumerGroupRecord{ID: key.ID, Group: group})
		}
	}
	if len(consumed) != len(occurrences) {
		return nil, errConsumerPreparationFailed
	}

	bindings, err := runtime.NewConsumerBindings(consumers, groups, credentials)
	if err != nil {
		return nil, errConsumerPreparationFailed
	}
	return bindings, nil
}

func (preparer *consumerBindingPreparer) prepareConsumer(
	ctx context.Context,
	attempt PreparationAttempt,
	normalized normalizedResource,
	occurrences map[consumerOccurrenceKey]FactoryOccurrence,
) (runtime.ConsumerRecord, []runtime.ConsumerCredentialBinding, []consumerOccurrenceKey, error) {
	bindings := make([]runtime.ConsumerCredentialBinding, 0, len(normalized.view.plugins))
	used := make([]consumerOccurrenceKey, 0, len(normalized.view.plugins))
	for _, factory := range sortedFactories(normalized.view.plugins) {
		key := consumerOccurrenceKey{resource: normalized.key, factory: factory}
		occurrence, exists := occurrences[key]
		if !exists {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		used = append(used, key)
		if !consumerregistry.Supports(factory) {
			continue
		}
		config := normalized.view.plugins[factory]
		if err := preparer.catalog.TransformDeclaredFields(
			factory,
			capability.SecretConsumerConfig,
			config,
			func(declaration capability.SecretDeclaration, _ string, raw any) (any, error) {
				reference, ok := raw.(string)
				if !ok {
					return raw, nil
				}
				resolved, err := attempt.MaterializeSecret(ctx, occurrence, declaration.Field, reference)
				if err != nil {
					return raw, errConsumerPreparationFailed
				}
				var plaintext string
				if err := resolved.Use(func(value string) error {
					plaintext = value
					return nil
				}); err != nil {
					return raw, errConsumerPreparationFailed
				}
				return plaintext, nil
			},
		); err != nil {
			return runtime.ConsumerRecord{}, nil, nil, err
		}
		if err := consumerregistry.ValidateResolved(factory, config); err != nil {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		lookupKey, err := consumerregistry.LookupKey(factory, config)
		if err != nil {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		bindings = append(bindings, runtime.ConsumerCredentialBinding{
			Plugin: factory, Key: lookupKey, ConsumerID: normalized.key.ID,
		})
	}

	var consumer resource.Consumer
	if err := util.Parse(normalized.document, &consumer); err != nil ||
		consumer.Username != normalized.key.ID {
		return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
	}
	consumer.ConfigDigest = sha256.Sum256(normalized.raw)
	return runtime.ConsumerRecord{ID: normalized.key.ID, Consumer: consumer}, bindings, used, nil
}

func indexConsumerOccurrences(
	attempt PreparationAttempt,
) (map[consumerOccurrenceKey]FactoryOccurrence, error) {
	indexed := make(map[consumerOccurrenceKey]FactoryOccurrence)
	for _, occurrence := range attempt.Occurrences(capability.SecretConsumerConfig) {
		if occurrence.Domain() != generation.DomainHTTP || occurrence.Resource().Kind != "consumers" {
			return nil, errConsumerPreparationFailed
		}
		key := consumerOccurrenceKey{resource: occurrence.Resource(), factory: occurrence.Factory()}
		if _, exists := indexed[key]; exists {
			return nil, errConsumerPreparationFailed
		}
		indexed[key] = occurrence
	}
	return indexed, nil
}

func validateConsumerOccurrenceSet(
	input normalizedInput,
	occurrences map[consumerOccurrenceKey]FactoryOccurrence,
) error {
	expected := make(map[consumerOccurrenceKey]struct{})
	for _, resourceKey := range input.keys() {
		if resourceKey.Kind != "consumers" {
			continue
		}
		for _, factory := range sortedFactories(input.resources[resourceKey].view.plugins) {
			expected[consumerOccurrenceKey{resource: resourceKey, factory: factory}] = struct{}{}
		}
	}
	if len(expected) != len(occurrences) {
		return errConsumerPreparationFailed
	}
	for key := range expected {
		if _, exists := occurrences[key]; !exists {
			return errConsumerPreparationFailed
		}
	}
	return nil
}

func consumerPreparationError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errConsumerPreparationFailed
}
