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

type staticConsumerCredentialKey struct {
	plugin string
	key    string
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
	preparation PreparationGeneration,
) (*runtime.ConsumerBindings, error) {
	if ctx == nil || preparer == nil || preparer.catalog == nil ||
		!preparation.secrets.Valid() ||
		preparation.Generation() == 0 || preparation.Generation() != preparation.secrets.Generation() {
		return nil, errConsumerPreparationFailed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	occurrences, err := indexConsumerOccurrences(preparation)
	if err != nil {
		return nil, errConsumerPreparationFailed
	}
	candidate, exists := preparation.Candidate(generation.DomainHTTP)
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
	credentialIndexes := make(map[staticConsumerCredentialKey]int)
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
				preparation,
				normalized,
				occurrences,
			)
			if prepareErr != nil {
				return nil, consumerPreparationError(ctx)
			}
			consumers = append(consumers, consumerRecord)
			for _, binding := range bindings {
				key := staticConsumerCredentialKey{plugin: binding.Plugin, key: binding.Key}
				if index, exists := credentialIndexes[key]; exists {
					credentials[index] = binding
					continue
				}
				credentialIndexes[key] = len(credentials)
				credentials = append(credentials, binding)
			}
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
	preparation PreparationGeneration,
	normalized normalizedResource,
	occurrences map[consumerOccurrenceKey]FactoryOccurrence,
) (runtime.ConsumerRecord, []runtime.ConsumerCredentialBinding, []consumerOccurrenceKey, error) {
	bindings := make([]runtime.ConsumerCredentialBinding, 0, len(normalized.view.plugins))
	used := make([]consumerOccurrenceKey, 0, len(normalized.view.plugins))
	for _, factory := range sortedFactories(normalized.view.plugins) {
		config := normalized.view.plugins[factory]
		if pluginConfigDisabled(config) {
			continue
		}
		key := consumerOccurrenceKey{resource: normalized.key, factory: factory}
		_, exists := occurrences[key]
		if !exists {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		used = append(used, key)
		if !consumerregistry.Supports(factory) {
			continue
		}
		declaresSecret := false
		if err := preparer.catalog.TransformDeclaredFields(
			factory,
			capability.SecretConsumerConfig,
			config,
			func(_ capability.SecretDeclaration, _ string, raw any) (any, error) {
				if value, ok := raw.(string); ok && capability.IsMaterializableSecretEnvelope(value) {
					declaresSecret = true
				}
				return raw, nil
			},
		); err != nil {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		if !declaresSecret {
			if err := consumerregistry.ValidateResolved(factory, config); err != nil {
				return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
			}
		}
		lookupKey, err := consumerregistry.LookupKey(factory, config)
		if err != nil {
			return runtime.ConsumerRecord{}, nil, nil, errConsumerPreparationFailed
		}
		if capability.IsMaterializableSecretEnvelope(lookupKey) {
			continue
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
	consumer.ID = normalized.key.ID
	consumer.ConsumerName = ""
	consumer.AuthConf = nil
	consumer.CredentialID = ""
	consumer.CustomID = ""
	consumer.ConfigDigest = sha256.Sum256(normalized.raw)
	return runtime.ConsumerRecord{ID: normalized.key.ID, Consumer: consumer}, bindings, used, nil
}

func indexConsumerOccurrences(
	preparation PreparationGeneration,
) (map[consumerOccurrenceKey]FactoryOccurrence, error) {
	indexed := make(map[consumerOccurrenceKey]FactoryOccurrence)
	for _, occurrence := range preparation.Occurrences(capability.SecretConsumerConfig) {
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
			if pluginConfigDisabled(input.resources[resourceKey].view.plugins[factory]) {
				continue
			}
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
