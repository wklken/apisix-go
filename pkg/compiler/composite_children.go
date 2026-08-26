package compiler

import (
	"context"
	"fmt"
	"sort"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
)

type compositeChildOccurrenceSpec struct {
	outer    factoryOccurrenceSpec
	factory  string
	position string
	config   map[string]any
}

type compositeChildOccurrence struct {
	outer    FactoryOccurrence
	factory  string
	position string
	config   map[string]any
}

func compositeChildOccurrenceSpecsFromCandidates(
	ctx context.Context,
	candidates map[generation.Domain]generation.PublicationCandidate,
	outerOccurrences []factoryOccurrenceSpec,
) ([]compositeChildOccurrenceSpec, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: composite child inventory context is required", ErrInvalidInput)
	}
	allowedOuters := make(map[factoryOccurrenceSpec]struct{}, len(outerOccurrences))
	for _, occurrence := range outerOccurrences {
		if occurrence.source != capability.SecretPluginConfig ||
			(occurrence.factory != "workflow" && occurrence.factory != "multi-auth") {
			continue
		}
		if _, duplicate := allowedOuters[occurrence]; duplicate {
			return nil, generation.ErrIntegrity
		}
		allowedOuters[occurrence] = struct{}{}
	}
	children := make([]compositeChildOccurrenceSpec, 0)
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		candidate, exists := candidates[domain]
		if !exists {
			continue
		}
		if err := generation.ValidatePublicationCandidate(
			domain, candidate.Artifact.Revision, candidate,
		); err != nil {
			return nil, err
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
			if !regularPluginResourceKind(key.Kind) {
				continue
			}
			resource := input.resources[key]
			for _, outerFactory := range sortedFactories(resource.view.plugins) {
				if outerFactory != "workflow" && outerFactory != "multi-auth" {
					continue
				}
				outer := factoryOccurrenceSpec{
					domain: domain, resource: key,
					source: capability.SecretPluginConfig, factory: outerFactory,
				}
				if _, allowed := allowedOuters[outer]; !allowed {
					continue
				}
				config, enabled, err := activeCompositeConfig(resource.view.plugins[outerFactory])
				if err != nil {
					return nil, generation.ErrIntegrity
				}
				if !enabled {
					continue
				}
				var nested []compositeChildOccurrenceSpec
				switch outerFactory {
				case "workflow":
					nested, err = workflowChildOccurrenceSpecs(outer, config)
				case "multi-auth":
					nested, err = multiAuthChildOccurrenceSpecs(outer, config)
				}
				if err != nil {
					return nil, generation.ErrIntegrity
				}
				children = append(children, nested...)
			}
		}
	}
	return children, nil
}

func activeCompositeConfig(value any) (map[string]any, bool, error) {
	config, ok := value.(map[string]any)
	if !ok {
		return nil, false, generation.ErrIntegrity
	}
	clone := cloneCompositeJSONMap(config)
	metadataValue, hasMetadata := clone["_meta"]
	delete(clone, "_meta")
	if !hasMetadata {
		return clone, true, nil
	}
	metadata, ok := metadataValue.(map[string]any)
	if !ok {
		return nil, false, generation.ErrIntegrity
	}
	disabledValue, hasDisabled := metadata["disable"]
	if !hasDisabled {
		return clone, true, nil
	}
	disabled, ok := disabledValue.(bool)
	if !ok {
		return nil, false, generation.ErrIntegrity
	}
	return clone, !disabled, nil
}

func workflowChildOccurrenceSpecs(
	outer factoryOccurrenceSpec,
	config map[string]any,
) ([]compositeChildOccurrenceSpec, error) {
	rules, ok := config["rules"].([]any)
	if !ok {
		return nil, generation.ErrIntegrity
	}
	children := make([]compositeChildOccurrenceSpec, 0)
	for ruleIndex, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			return nil, generation.ErrIntegrity
		}
		actions, ok := rule["actions"].([]any)
		if !ok {
			return nil, generation.ErrIntegrity
		}
		for actionIndex, rawAction := range actions {
			action, ok := rawAction.([]any)
			if !ok || len(action) == 0 {
				return nil, generation.ErrIntegrity
			}
			factory, ok := action[0].(string)
			if !ok || factory == "" {
				return nil, generation.ErrIntegrity
			}
			if factory == "return" {
				continue
			}
			switch factory {
			case "limit-req", "limit-conn", "limit-count":
			default:
				return nil, generation.ErrIntegrity
			}
			childConfig := map[string]any{}
			if len(action) > 1 {
				childConfig, ok = action[1].(map[string]any)
				if !ok {
					return nil, generation.ErrIntegrity
				}
			}
			children = append(children, compositeChildOccurrenceSpec{
				outer: outer, factory: factory,
				position: fmt.Sprintf("rules[%d].actions[%d]", ruleIndex, actionIndex),
				config:   cloneCompositeJSONMap(childConfig),
			})
		}
	}
	return children, nil
}

func multiAuthChildOccurrenceSpecs(
	outer factoryOccurrenceSpec,
	config map[string]any,
) ([]compositeChildOccurrenceSpec, error) {
	plugins, ok := config["auth_plugins"].([]any)
	if !ok || len(plugins) < 2 {
		return nil, generation.ErrIntegrity
	}
	children := make([]compositeChildOccurrenceSpec, 0)
	for pluginIndex, rawPlugin := range plugins {
		pluginConfig, ok := rawPlugin.(map[string]any)
		if !ok || len(pluginConfig) == 0 {
			return nil, generation.ErrIntegrity
		}
		factories := make([]string, 0, len(pluginConfig))
		for factory := range pluginConfig {
			factories = append(factories, factory)
		}
		sort.Strings(factories)
		for _, factory := range factories {
			if !supportedMultiAuthChildFactory(factory) {
				return nil, generation.ErrIntegrity
			}
			childConfig, ok := pluginConfig[factory].(map[string]any)
			if !ok {
				return nil, generation.ErrIntegrity
			}
			children = append(children, compositeChildOccurrenceSpec{
				outer: outer, factory: factory,
				position: fmt.Sprintf("auth_plugins[%d].%s", pluginIndex, factory),
				config:   cloneCompositeJSONMap(childConfig),
			})
		}
	}
	return children, nil
}

func supportedMultiAuthChildFactory(factory string) bool {
	switch factory {
	case "basic-auth", "key-auth", "jwt-auth", "hmac-auth", "ldap-auth", "jwe-decrypt", "wolf-rbac":
		return true
	default:
		return false
	}
}

func validateCompositeScopedSecretSupport(
	children []compositeChildOccurrenceSpec,
	catalog *capability.SecretDeclarationCatalog,
) error {
	if catalog == nil {
		return fmt.Errorf("%w: secret declaration catalog is required", ErrInvalidInput)
	}
	required := make(map[string]struct{})
	for _, child := range children {
		requiresPluginOwner := false
		catalog.ForEach(child.factory, capability.SecretPluginConfig, func(
			declaration capability.SecretDeclaration,
		) {
			if declaration.EffectiveTarget() == capability.SecretMaterializationPlugin {
				requiresPluginOwner = true
			}
		})
		if requiresPluginOwner {
			required[child.factory] = struct{}{}
		}
	}
	for _, factory := range sortedFactoriesFromSet(required) {
		supported, err := plugin.SupportsScopedSecretMaterialization(factory)
		if err != nil || !supported {
			return fmt.Errorf("%w: scoped secret preparation is unavailable", ErrInvalidInput)
		}
	}
	return nil
}

func bindCompositeChildOccurrences(
	attempt PreparationAttempt,
	specs []compositeChildOccurrenceSpec,
) ([]compositeChildOccurrence, error) {
	if attempt.authority == nil || !attempt.capability.Valid() {
		return nil, fmt.Errorf("%w: composite child attempt is invalid", ErrInvalidInput)
	}
	topLevel := make(map[factoryOccurrenceSpec]FactoryOccurrence, len(attempt.occurrences))
	for _, occurrence := range attempt.occurrences {
		if !attempt.owns(occurrence) {
			return nil, fmt.Errorf("%w: composite child outer authority is invalid", ErrInvalidInput)
		}
		key := factoryOccurrenceSpec{
			domain: occurrence.domain, resource: occurrence.resource,
			source: occurrence.source, factory: occurrence.factory,
		}
		if _, duplicate := topLevel[key]; duplicate {
			return nil, fmt.Errorf("%w: composite child outer authority is duplicated", ErrInvalidInput)
		}
		topLevel[key] = occurrence
	}
	seen := make(map[string]struct{}, len(specs))
	bound := make([]compositeChildOccurrence, 0, len(specs))
	for _, spec := range specs {
		outer, exists := topLevel[spec.outer]
		if !exists || spec.factory == "" || spec.position == "" || spec.config == nil {
			return nil, fmt.Errorf("%w: composite child occurrence is invalid", ErrInvalidInput)
		}
		identity := fmt.Sprintf(
			"%s\x00%s\x00%s\x00%s\x00%s\x00%s",
			spec.outer.domain, spec.outer.resource.Kind, spec.outer.resource.ID,
			spec.outer.factory, spec.factory, spec.position,
		)
		if _, duplicate := seen[identity]; duplicate {
			return nil, fmt.Errorf("%w: composite child occurrence is duplicated", ErrInvalidInput)
		}
		seen[identity] = struct{}{}
		bound = append(bound, compositeChildOccurrence{
			outer: outer, factory: spec.factory, position: spec.position,
			config: cloneCompositeJSONMap(spec.config),
		})
	}
	return bound, nil
}

func cloneCompositeJSONMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = cloneCompositeJSONValue(value)
	}
	return clone
}

func cloneCompositeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCompositeJSONMap(typed)
	case []any:
		clone := make([]any, len(typed))
		for index, item := range typed {
			clone[index] = cloneCompositeJSONValue(item)
		}
		return clone
	default:
		return value
	}
}
