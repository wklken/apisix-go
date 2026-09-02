package compiler

import (
	"context"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
	consumerregistry "github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/util"
)

type consumerCredentialCandidate struct {
	consumerID string
	occurrence FactoryOccurrence
}

func newConsumerLookupView(
	bindings *runtime.ConsumerBindings,
	preparation PreparationGeneration,
	catalog *capability.SecretDeclarationCatalog,
) consumerLookupView {
	view := consumerLookupView{
		bindings: bindings, preparation: preparation, catalog: catalog,
		candidates: make(map[string][]consumerCredentialCandidate),
	}
	for _, occurrence := range preparation.Occurrences(capability.SecretConsumerConfig) {
		if occurrence.Resource().Kind != "consumers" || !consumerregistry.Supports(occurrence.Factory()) {
			continue
		}
		view.candidates[occurrence.Factory()] = append(
			view.candidates[occurrence.Factory()],
			consumerCredentialCandidate{consumerID: occurrence.Resource().ID, occurrence: occurrence},
		)
	}
	for factory := range view.candidates {
		sort.Slice(view.candidates[factory], func(left, right int) bool {
			return view.candidates[factory][left].consumerID < view.candidates[factory][right].consumerID
		})
	}
	return view
}

func (view consumerLookupView) UseConsumerCredential(
	ctx context.Context,
	plugin string,
	key string,
	use base.ConsumerCredentialUse,
) (bool, error) {
	if view.bindings == nil || view.catalog == nil || ctx == nil || use == nil {
		return false, nil
	}
	if consumer, found := view.bindings.ConsumerByPluginKey(plugin, key); found {
		candidate, ok := view.candidate(plugin, consumer.ID)
		if !ok {
			return true, use(consumer, consumer.Plugins[plugin])
		}
		resolved, matches, err := view.resolveCandidate(ctx, plugin, key, consumer, candidate)
		if err != nil || !matches {
			return matches, err
		}
		return true, use(consumer, resolved)
	}

	var matchedConsumer resource.Consumer
	var matchedConfig resource.PluginConfig
	matched := false
	for _, candidate := range view.candidates[plugin] {
		consumer, found := view.bindings.ConsumerByID(candidate.consumerID)
		if !found {
			continue
		}
		resolved, matches, err := view.resolveCandidate(ctx, plugin, key, consumer, candidate)
		if err != nil {
			return false, err
		}
		if !matches {
			continue
		}
		if matched {
			return false, secret.ErrCredentialUnavailable
		}
		matched = true
		matchedConsumer = consumer
		matchedConfig = resolved
	}
	if matched {
		return true, use(matchedConsumer, matchedConfig)
	}
	return false, nil
}

func (view consumerLookupView) candidate(plugin string, consumerID string) (consumerCredentialCandidate, bool) {
	for _, candidate := range view.candidates[plugin] {
		if candidate.consumerID == consumerID {
			return candidate, true
		}
	}
	return consumerCredentialCandidate{}, false
}

func (view consumerLookupView) resolveCandidate(
	ctx context.Context,
	plugin string,
	key string,
	consumer resource.Consumer,
	candidate consumerCredentialCandidate,
) (resource.PluginConfig, bool, error) {
	raw, exists := consumer.Plugins[plugin]
	if !exists {
		return nil, false, nil
	}
	var resolved map[string]any
	if err := util.Parse(raw, &resolved); err != nil {
		return nil, false, secret.ErrCredentialUnavailable
	}
	if err := view.catalog.TransformDeclaredFields(
		plugin,
		capability.SecretConsumerConfig,
		resolved,
		func(declaration capability.SecretDeclaration, _ string, raw any) (any, error) {
			reference, ok := raw.(string)
			if !ok {
				return raw, nil
			}
			value, err := view.preparation.MaterializeSecret(
				ctx, candidate.occurrence, declaration.Field, reference,
			)
			if err != nil {
				return raw, secret.ErrCredentialUnavailable
			}
			var plaintext string
			if err := value.Use(func(resolved string) error {
				plaintext = resolved
				return nil
			}); err != nil {
				return raw, secret.ErrCredentialUnavailable
			}
			return plaintext, nil
		},
	); err != nil {
		return nil, false, err
	}
	if err := consumerregistry.ValidateResolved(plugin, resolved); err != nil {
		return nil, false, secret.ErrCredentialUnavailable
	}
	lookupKey, err := consumerregistry.LookupKey(plugin, resolved)
	if err != nil {
		return nil, false, secret.ErrCredentialUnavailable
	}
	if lookupKey != key {
		return nil, false, nil
	}
	return resolved, true, nil
}
