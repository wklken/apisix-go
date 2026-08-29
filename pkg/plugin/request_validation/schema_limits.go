package request_validation

import (
	"errors"
	"net/url"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

const (
	requestValidationMaxSchemaBytes      = 256 * 1024
	requestValidationMaxSchemaDepth      = 64
	requestValidationMaxSchemaNodes      = 8192
	requestValidationMaxSchemaReferences = 256
)

var (
	errRequestValidationSchemaBudget      = errors.New("request-validation schema resource budget exceeded")
	errRequestValidationExternalReference = errors.New("request-validation external schema reference is unavailable")
)

type requestValidationSchemaBudget struct {
	nodes int
}

func validateRequestValidationSchemaDocument(
	document map[string]any, allowSecretEnvelopes bool,
) error {
	budget := requestValidationSchemaBudget{}
	if err := budget.visit(document, 1); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil || len(encoded) > requestValidationMaxSchemaBytes {
		return errRequestValidationSchemaBudget
	}
	if allowSecretEnvelopes {
		// Reference semantics are checked after every terminal secret has been
		// restored, while still inside Value.Use and before schema compilation.
		return nil
	}
	return validateRequestValidationSchemaReferences(document)
}

func (budget *requestValidationSchemaBudget) visit(value any, depth int) error {
	if depth > requestValidationMaxSchemaDepth {
		return errRequestValidationSchemaBudget
	}
	budget.nodes++
	if budget.nodes > requestValidationMaxSchemaNodes {
		return errRequestValidationSchemaBudget
	}
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			if err := budget.visit(child, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := budget.visit(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

type requestValidationSchemaDraft uint16

const (
	requestValidationDraft4    requestValidationSchemaDraft = 4
	requestValidationDraft6    requestValidationSchemaDraft = 6
	requestValidationDraft7    requestValidationSchemaDraft = 7
	requestValidationDraft2019 requestValidationSchemaDraft = 2019
	requestValidationDraft2020 requestValidationSchemaDraft = 2020
)

type requestValidationSubschemaPosition uint8

const (
	requestValidationSubschemaSelf requestValidationSubschemaPosition = 1 << iota
	requestValidationSubschemaItem
	requestValidationSubschemaProperty
)

type requestValidationSchemaReference struct {
	base string
	raw  string
}

type requestValidationSchemaReferenceScan struct {
	draft      requestValidationSchemaDraft
	resources  map[string]struct{}
	references []requestValidationSchemaReference
}

const requestValidationSchemaRoot = "https://request-validation.invalid/schema.json"

func validateRequestValidationSchemaReferences(document map[string]any) error {
	draft, err := requestValidationBuiltinSchemaDraft(document["$schema"])
	if err != nil {
		return err
	}
	scan := requestValidationSchemaReferenceScan{
		draft: draft, resources: map[string]struct{}{requestValidationSchemaRoot: {}},
	}
	if err := scan.visit(document, requestValidationSchemaRoot); err != nil {
		return err
	}
	for _, reference := range scan.references {
		resolved, err := requestValidationResolveSchemaURL(reference.base, reference.raw)
		if err != nil {
			return errRequestValidationExternalReference
		}
		resource, _ := requestValidationSplitSchemaURL(resolved)
		if _, ok := scan.resources[resource]; !ok {
			return errRequestValidationExternalReference
		}
	}
	return nil
}

func (scan *requestValidationSchemaReferenceScan) visit(value any, base string) error {
	schema, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if id, ok := schema[scan.idKeyword()].(string); ok &&
		(scan.draft > requestValidationDraft7 || schema["$ref"] == nil) {
		resolved, err := requestValidationResolveSchemaURL(base, id)
		if err != nil {
			return errRequestValidationExternalReference
		}
		base, _ = requestValidationSplitSchemaURL(resolved)
		if base != "" {
			scan.resources[base] = struct{}{}
		}
	}
	for _, keyword := range scan.referenceKeywords() {
		if reference, ok := schema[keyword].(string); ok {
			scan.references = append(scan.references, requestValidationSchemaReference{
				base: base, raw: reference,
			})
			if len(scan.references) > requestValidationMaxSchemaReferences {
				return errRequestValidationSchemaBudget
			}
		}
	}
	for keyword, position := range scan.subschemaPositions() {
		child, ok := schema[keyword]
		if !ok {
			continue
		}
		if position&requestValidationSubschemaSelf != 0 {
			if err := scan.visit(child, base); err != nil {
				return err
			}
		}
		if position&requestValidationSubschemaItem != 0 {
			if items, ok := child.([]any); ok {
				for _, item := range items {
					if err := scan.visit(item, base); err != nil {
						return err
					}
				}
			}
		}
		if position&requestValidationSubschemaProperty != 0 {
			if properties, ok := child.(map[string]any); ok {
				for _, property := range properties {
					if err := scan.visit(property, base); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (scan requestValidationSchemaReferenceScan) idKeyword() string {
	if scan.draft == requestValidationDraft4 {
		return "id"
	}
	return "$id"
}

func (scan requestValidationSchemaReferenceScan) referenceKeywords() []string {
	keywords := []string{"$ref"}
	if scan.draft >= requestValidationDraft2019 {
		keywords = append(keywords, "$recursiveRef")
	}
	if scan.draft >= requestValidationDraft2020 {
		keywords = append(keywords, "$dynamicRef")
	}
	return keywords
}

func (scan requestValidationSchemaReferenceScan) subschemaPositions() map[string]requestValidationSubschemaPosition {
	positions := map[string]requestValidationSubschemaPosition{
		"definitions":          requestValidationSubschemaProperty,
		"not":                  requestValidationSubschemaSelf,
		"allOf":                requestValidationSubschemaItem,
		"anyOf":                requestValidationSubschemaItem,
		"oneOf":                requestValidationSubschemaItem,
		"properties":           requestValidationSubschemaProperty,
		"additionalProperties": requestValidationSubschemaSelf,
		"patternProperties":    requestValidationSubschemaProperty,
		"items":                requestValidationSubschemaSelf | requestValidationSubschemaItem,
		"additionalItems":      requestValidationSubschemaSelf,
		"dependencies":         requestValidationSubschemaProperty,
	}
	if scan.draft >= requestValidationDraft6 {
		positions["propertyNames"] = requestValidationSubschemaSelf
		positions["contains"] = requestValidationSubschemaSelf
	}
	if scan.draft >= requestValidationDraft7 {
		positions["if"] = requestValidationSubschemaSelf
		positions["then"] = requestValidationSubschemaSelf
		positions["else"] = requestValidationSubschemaSelf
	}
	if scan.draft >= requestValidationDraft2019 {
		positions["$defs"] = requestValidationSubschemaProperty
		positions["dependentSchemas"] = requestValidationSubschemaProperty
		positions["unevaluatedProperties"] = requestValidationSubschemaSelf
		positions["unevaluatedItems"] = requestValidationSubschemaSelf
		positions["contentSchema"] = requestValidationSubschemaSelf
	}
	if scan.draft >= requestValidationDraft2020 {
		positions["prefixItems"] = requestValidationSubschemaItem
	}
	return positions
}

func requestValidationBuiltinSchemaDraft(value any) (requestValidationSchemaDraft, error) {
	schemaURL, ok := value.(string)
	if !ok {
		return requestValidationDraft2020, nil
	}
	if normalized, ok := strings.CutPrefix(schemaURL, "http://"); ok {
		schemaURL = "https://" + normalized
	}
	schemaURL = strings.TrimSuffix(schemaURL, "#/")
	schemaURL = strings.TrimSuffix(schemaURL, "#")
	switch schemaURL {
	case "https://json-schema.org/schema", "https://json-schema.org/draft/2020-12/schema":
		return requestValidationDraft2020, nil
	case "https://json-schema.org/draft/2019-09/schema":
		return requestValidationDraft2019, nil
	case "https://json-schema.org/draft-07/schema":
		return requestValidationDraft7, nil
	case "https://json-schema.org/draft-06/schema":
		return requestValidationDraft6, nil
	case "https://json-schema.org/draft-04/schema":
		return requestValidationDraft4, nil
	default:
		return 0, errRequestValidationExternalReference
	}
}

func requestValidationResolveSchemaURL(base, reference string) (string, error) {
	if reference == "" {
		return base, nil
	}
	if strings.HasPrefix(reference, "urn:") {
		return reference, nil
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		return reference, nil
	}
	if strings.HasPrefix(base, "urn:") {
		base, _ = requestValidationSplitSchemaURL(base)
		return base + reference, nil
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	return baseURL.ResolveReference(parsed).String(), nil
}

func requestValidationSplitSchemaURL(value string) (string, string) {
	index := strings.IndexByte(value, '#')
	if index < 0 {
		return value, "#"
	}
	fragment := value[index:]
	if fragment == "#/" {
		fragment = "#"
	}
	return value[:index], fragment
}
