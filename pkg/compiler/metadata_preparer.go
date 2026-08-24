package compiler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/runtime"
)

var errMetadataPreparationFailed = fmt.Errorf(
	"%w: metadata preparation failed",
	ErrInvalidInput,
)

type metadataPreparer struct {
	schemas *schemaSet
}

type metadataOccurrenceKey struct {
	resource generation.ResourceKey
	factory  string
}

func newMetadataPreparer(schemas *schemaSet) (*metadataPreparer, error) {
	if schemas == nil || schemas.catalog == nil {
		return nil, errMetadataPreparationFailed
	}
	return &metadataPreparer{schemas: schemas}, nil
}

func (preparer *metadataPreparer) PrepareMetadata(
	ctx context.Context,
	attempt PreparationAttempt,
) (runtime.MetadataView, error) {
	if ctx == nil || preparer == nil || preparer.schemas == nil || preparer.schemas.catalog == nil ||
		attempt.authority == nil || !attempt.capability.Valid() || attempt.Generation() == 0 ||
		attempt.Generation() != attempt.capability.Generation() {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}
	if err := ctx.Err(); err != nil {
		return runtime.MetadataView{}, err
	}

	occurrences, err := indexMetadataOccurrences(attempt)
	if err != nil {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}
	candidate, exists := attempt.Candidate(generation.DomainHTTP)
	if !exists {
		if len(occurrences) != 0 {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		return runtime.NewMetadataView(nil)
	}
	if err := generation.ValidatePublicationCandidate(
		generation.DomainHTTP,
		candidate.Artifact.Revision,
		candidate,
	); err != nil {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}

	input, issues, err := normalizeContext(ctx, candidate.Snapshot)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return runtime.MetadataView{}, ctxErr
		}
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}
	if len(issues) != 0 {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}

	expected := make(map[metadataOccurrenceKey]struct{})
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return runtime.MetadataView{}, err
		}
		if key.Kind == "plugin_metadata" {
			expected[metadataOccurrenceKey{resource: key, factory: key.ID}] = struct{}{}
		}
	}
	if !equalMetadataOccurrenceSet(expected, occurrences) {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}

	documents := make(map[string][]byte, len(expected))
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return runtime.MetadataView{}, err
		}
		if key.Kind != "plugin_metadata" {
			continue
		}
		normalized := input.resources[key]
		entry, found := preparer.schemas.factories[key.ID]
		if !found || entry.metadata == nil {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		document, err := cloneMetadataDocument(normalized.document)
		if err != nil {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		if !metadataDeclaredTerminalsAreStrings(
			preparer.schemas.catalog,
			key.ID,
			document,
		) {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		occurrence := occurrences[metadataOccurrenceKey{resource: key, factory: key.ID}]
		if err := preparer.schemas.catalog.TransformDeclaredFields(
			key.ID,
			capability.SecretPluginMetadata,
			document,
			func(declaration capability.SecretDeclaration, _ string, raw any) (any, error) {
				if err := ctx.Err(); err != nil {
					return raw, err
				}
				reference, ok := raw.(string)
				if !ok {
					return raw, errMetadataPreparationFailed
				}
				value, err := attempt.MaterializeSecret(ctx, occurrence, declaration.Field, reference)
				if err != nil {
					return raw, errMetadataPreparationFailed
				}
				var plaintext string
				if err := value.Use(func(resolved string) error {
					plaintext = resolved
					return nil
				}); err != nil {
					return raw, errMetadataPreparationFailed
				}
				return plaintext, nil
			},
		); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return runtime.MetadataView{}, ctxErr
			}
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		if err := ctx.Err(); err != nil {
			return runtime.MetadataView{}, err
		}
		if entry.metadata.Validate(document) != nil {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		raw, err := json.Marshal(document)
		if err != nil {
			return runtime.MetadataView{}, errMetadataPreparationFailed
		}
		documents[key.ID] = raw
	}

	view, err := runtime.NewMetadataView(documents)
	if err != nil {
		return runtime.MetadataView{}, errMetadataPreparationFailed
	}
	return view, nil
}

func indexMetadataOccurrences(
	attempt PreparationAttempt,
) (map[metadataOccurrenceKey]FactoryOccurrence, error) {
	indexed := make(map[metadataOccurrenceKey]FactoryOccurrence)
	for _, occurrence := range attempt.Occurrences(capability.SecretPluginMetadata) {
		if !attempt.owns(occurrence) || occurrence.Domain() != generation.DomainHTTP ||
			occurrence.Resource().Kind != "plugin_metadata" ||
			occurrence.Resource().ID != occurrence.Factory() {
			return nil, errMetadataPreparationFailed
		}
		key := metadataOccurrenceKey{resource: occurrence.Resource(), factory: occurrence.Factory()}
		if _, exists := indexed[key]; exists {
			return nil, errMetadataPreparationFailed
		}
		indexed[key] = occurrence
	}
	return indexed, nil
}

func equalMetadataOccurrenceSet(
	expected map[metadataOccurrenceKey]struct{},
	occurrences map[metadataOccurrenceKey]FactoryOccurrence,
) bool {
	if len(expected) != len(occurrences) {
		return false
	}
	for key := range expected {
		if _, exists := occurrences[key]; !exists {
			return false
		}
	}
	return true
}

func cloneMetadataDocument(document any) (any, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return decodeExactDocument(encoded)
}

func metadataDeclaredTerminalsAreStrings(
	catalog *capability.SecretDeclarationCatalog,
	factory string,
	document any,
) bool {
	valid := true
	catalog.ForEach(factory, capability.SecretPluginMetadata, func(
		declaration capability.SecretDeclaration,
	) {
		if !valid {
			return
		}
		_, valid = metadataDeclaredPathHasStringTerminal(document, strings.Split(declaration.Field, "."))
	})
	return valid
}

func metadataDeclaredPathHasStringTerminal(current any, segments []string) (bool, bool) {
	if len(segments) == 0 {
		_, ok := current.(string)
		return true, ok
	}

	segment := segments[0]
	switch value := current.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			if segment == "*" || strings.EqualFold(key, segment) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		found := false
		for _, key := range keys {
			childFound, valid := metadataDeclaredPathHasStringTerminal(value[key], segments[1:])
			found = found || childFound
			if !valid {
				return true, false
			}
		}
		return found, true
	case []any:
		if segment != "*" {
			return false, true
		}
		found := false
		for _, child := range value {
			childFound, valid := metadataDeclaredPathHasStringTerminal(child, segments[1:])
			found = found || childFound
			if !valid {
				return true, false
			}
		}
		return found, true
	default:
		return false, true
	}
}

var _ MetadataPreparer = (*metadataPreparer)(nil)
