package compiler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

var errCompilerDiscardPreparationFailed = fmt.Errorf(
	"%w: compiler discard secret preparation failed",
	ErrInvalidInput,
)

func prepareCompilerDiscardSecrets(
	ctx context.Context,
	attempt PreparationAttempt,
	catalog *capability.SecretDeclarationCatalog,
) error {
	if ctx == nil || catalog == nil || attempt.authority == nil ||
		!attempt.capability.Valid() || attempt.Generation() == 0 ||
		attempt.Generation() != attempt.capability.Generation() {
		return errCompilerDiscardPreparationFailed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	inputs := make(map[generation.Domain]normalizedInput)
	for _, occurrence := range attempt.Occurrences(capability.SecretPluginConfig) {
		if !hasCompilerDiscardDeclaration(catalog, occurrence.Factory()) {
			continue
		}
		if !attempt.owns(occurrence) {
			return errCompilerDiscardPreparationFailed
		}
		input, exists := inputs[occurrence.Domain()]
		if !exists {
			candidate, found := attempt.Candidate(occurrence.Domain())
			if !found || generation.ValidatePublicationCandidate(
				occurrence.Domain(), candidate.Artifact.Revision, candidate,
			) != nil {
				return errCompilerDiscardPreparationFailed
			}
			var issues []resourceIssue
			var err error
			input, issues, err = normalizeContext(ctx, candidate.Snapshot)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return errCompilerDiscardPreparationFailed
			}
			if len(issues) != 0 {
				return errCompilerDiscardPreparationFailed
			}
			inputs[occurrence.Domain()] = input
		}

		normalized, exists := input.resources[occurrence.Resource()]
		if !exists {
			return errCompilerDiscardPreparationFailed
		}
		config, exists := normalized.view.plugins[occurrence.Factory()]
		if !exists {
			return errCompilerDiscardPreparationFailed
		}
		if err := validateCompilerDiscardTerminals(
			catalog, occurrence.Factory(), config,
		); err != nil {
			return errCompilerDiscardPreparationFailed
		}
		if err := catalog.TransformDeclaredFieldsForTarget(
			occurrence.Factory(),
			capability.SecretPluginConfig,
			capability.SecretMaterializationCompilerDiscard,
			config,
			func(declaration capability.SecretDeclaration, pointer string, raw any) (any, error) {
				if strings.Count(pointer, "/") != len(strings.Split(declaration.Field, ".")) {
					return raw, errCompilerDiscardPreparationFailed
				}
				reference, ok := raw.(string)
				if !ok {
					return raw, errCompilerDiscardPreparationFailed
				}
				if reference == "" {
					return raw, nil
				}
				value, err := attempt.MaterializeSecret(ctx, occurrence, declaration.Field, reference)
				if err != nil {
					return raw, errCompilerDiscardPreparationFailed
				}
				if err := value.Use(func(string) error { return nil }); err != nil {
					return raw, errCompilerDiscardPreparationFailed
				}
				return raw, nil
			},
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return errCompilerDiscardPreparationFailed
		}
	}
	return nil
}

func hasCompilerDiscardDeclaration(
	catalog *capability.SecretDeclarationCatalog,
	factory string,
) bool {
	if catalog == nil {
		return false
	}
	found := false
	catalog.ForEach(factory, capability.SecretPluginConfig, func(
		declaration capability.SecretDeclaration,
	) {
		if declaration.EffectiveTarget() == capability.SecretMaterializationCompilerDiscard {
			found = true
		}
	})
	return found
}

func validateCompilerDiscardTerminals(
	catalog *capability.SecretDeclarationCatalog,
	factory string,
	document any,
) error {
	valid := true
	catalog.ForEach(factory, capability.SecretPluginConfig, func(
		declaration capability.SecretDeclaration,
	) {
		if !valid || declaration.EffectiveTarget() != capability.SecretMaterializationCompilerDiscard {
			return
		}
		segments := strings.Split(declaration.Field, ".")
		if slices.Contains(segments, "*") {
			valid = false
			return
		}
		if !validCompilerDiscardPath(document, segments) {
			valid = false
		}
	})
	if !valid {
		return errCompilerDiscardPreparationFailed
	}
	return nil
}

func validCompilerDiscardPath(current any, segments []string) bool {
	if len(segments) == 0 {
		_, ok := current.(string)
		return ok
	}

	value, ok := current.(map[string]any)
	if !ok {
		return true
	}
	segment := segments[0]
	for key, child := range value {
		if !strings.EqualFold(key, segment) {
			continue
		}
		if len(segments) == 1 {
			if _, ok := child.(string); !ok {
				return false
			}
			continue
		}
		if !validCompilerDiscardPath(child, segments[1:]) {
			return false
		}
	}
	return true
}
