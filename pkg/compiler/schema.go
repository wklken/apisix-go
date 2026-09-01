package compiler

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/consumer"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	pluginSchemaInvalidCode         = "plugin-schema-invalid"
	pluginMetadataSchemaInvalidCode = "plugin-metadata-schema-invalid"
	consumerSchemaInvalidCode       = "consumer-schema-invalid"
)

type factorySchemas struct {
	config          *util.CompiledSchema
	metadata        *util.CompiledSchema
	consumer        *util.CompiledSchema
	consumerAllowed bool
	domains         []generation.Domain
}

type schemaSet struct {
	factories map[string]factorySchemas
	catalog   *capability.SecretDeclarationCatalog
}

func newSchemaSet() (*schemaSet, error) {
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		return nil, fmt.Errorf("secret declaration catalog: %w", err)
	}

	definitions := plugin.Definitions()
	set := &schemaSet{
		factories: make(map[string]factorySchemas, len(definitions)),
		catalog:   catalog,
	}
	for _, definition := range definitions {
		factory := definition.Factory
		if _, exists := set.factories[factory]; exists {
			return nil, fmt.Errorf("factory %q has duplicate schema ownership", factory)
		}
		witness, err := plugin.SchemaWitnessForFactory(factory)
		if err != nil {
			return nil, fmt.Errorf("factory %q schema witness is unavailable", factory)
		}
		if witness.Factory != factory {
			return nil, fmt.Errorf("factory %q schema witness does not match", factory)
		}
		config, err := compileFactorySchema(factory, "config", witness.Config, true)
		if err != nil {
			return nil, err
		}
		metadata, err := compileFactorySchema(factory, "metadata", witness.Metadata, false)
		if err != nil {
			return nil, err
		}

		consumerWitness, hasConsumerWitness := consumer.SchemaWitnessForFactory(factory)
		if hasConsumerWitness != witness.HasConsumer {
			return nil, fmt.Errorf("factory %q consumer schema capability does not match", factory)
		}
		var consumerSchema *util.CompiledSchema
		if witness.HasConsumer {
			if consumerWitness.Factory != factory {
				return nil, fmt.Errorf("factory %q consumer schema witness does not match", factory)
			}
			consumerSchema, err = compileFactorySchema(factory, "consumer", consumerWitness.Schema, true)
			if err != nil {
				return nil, err
			}
		}
		set.factories[factory] = factorySchemas{
			config: config, metadata: metadata, consumer: consumerSchema,
			consumerAllowed: slices.Contains(definition.Scopes, plugin.ScopeConsumer),
			domains:         schemaGenerationDomains(definition.Domain),
		}
	}
	return set, nil
}

func compileFactorySchema(
	factory string,
	source string,
	schema string,
	required bool,
) (*util.CompiledSchema, error) {
	if strings.TrimSpace(schema) == "" {
		if required {
			return nil, fmt.Errorf("factory %q %s schema is missing", factory, source)
		}
		return nil, nil
	}
	compiled, err := util.CompileSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("factory %q %s schema is invalid", factory, source)
	}
	return compiled, nil
}

func schemaGenerationDomains(domain plugin.Domain) []generation.Domain {
	switch domain {
	case plugin.DomainHTTP:
		return []generation.Domain{generation.DomainHTTP}
	case plugin.DomainStream:
		return []generation.Domain{generation.DomainStream}
	default:
		return nil
	}
}

func validateRawSchemas(
	resource normalizedResource,
	schemas *schemaSet,
	issues *[]resourceIssue,
	issuesByDomain map[generation.Domain][]resourceIssue,
) {
	if schemas == nil || resource.key.Kind == "plugins" {
		return
	}
	switch resource.key.Kind {
	case "plugin_metadata":
		entry, exists := schemas.factories[resource.key.ID]
		valid, diagnostic := false, ""
		if exists && entry.metadata != nil {
			valid, diagnostic = schemaAdmission(
				entry.metadata,
				schemas.catalog,
				resource.key.ID,
				capability.SecretPluginMetadata,
				resource.document,
			)
		}
		if !valid {
			issue := newIssue(
				resource.key, pluginMetadataSchemaInvalidCode, "plugin metadata schema validation failed",
			)
			issue.Diagnostic = diagnostic
			*issues = append(*issues, issue)
		}
	case "consumers":
		for factory, config := range resource.view.plugins {
			entry, exists := schemas.factories[factory]
			if !exists {
				continue
			}
			consumerSchema := entry.consumer
			if consumerSchema == nil && entry.consumerAllowed {
				consumerSchema = entry.config
			}
			valid, diagnostic := schemaAdmission(
				consumerSchema, schemas.catalog, factory, capability.SecretConsumerConfig, config,
			)
			if !valid {
				issue := newIssue(
					resource.key, consumerSchemaInvalidCode, "consumer schema validation failed",
				)
				issue.Diagnostic = diagnostic
				*issues = append(*issues, issue)
				return
			}
		}
	default:
		if !regularPluginResourceKind(resource.key.Kind) {
			return
		}
		resourceDomains := generation.DomainsForResourceKind(resource.key.Kind)
		for factory, config := range resource.view.plugins {
			entry, exists := schemas.factories[factory]
			if !exists {
				continue
			}
			valid, diagnostic := schemaAdmission(
				entry.config, schemas.catalog, factory, capability.SecretPluginConfig, config,
			)
			if valid {
				continue
			}
			issue := newIssue(resource.key, pluginSchemaInvalidCode, "plugin schema validation failed")
			issue.Diagnostic = diagnostic
			for _, domain := range entry.domains {
				if slices.Contains(resourceDomains, domain) {
					issuesByDomain[domain] = append(issuesByDomain[domain], issue)
				}
			}
		}
	}
}

func regularPluginResourceKind(kind string) bool {
	switch kind {
	case "routes", "stream_routes", "services", "global_rules", "plugin_configs", "consumer_groups":
		return true
	default:
		return false
	}
}

func schemaAdmission(
	compiled *util.CompiledSchema,
	catalog *capability.SecretDeclarationCatalog,
	factory string,
	source capability.SecretDeclarationSource,
	document any,
) (bool, string) {
	if compiled == nil {
		return false, ""
	}
	validationErr := compiled.Validate(document)
	if validationErr == nil {
		return true, ""
	}
	diagnostic := safeSchemaDiagnostic(factory, source, validationErr)

	admitted := make(map[string]struct{})
	if err := catalog.TransformDeclaredFields(
		factory,
		source,
		document,
		func(_ capability.SecretDeclaration, pointer string, value any) (any, error) {
			text, ok := value.(string)
			if ok && capability.IsMaterializableSecretEnvelope(text) {
				admitted[pointer] = struct{}{}
			}
			return value, nil
		},
	); err != nil || len(admitted) == 0 {
		return false, diagnostic
	}

	var schemaErr *jsonschema.ValidationError
	if !errors.As(validationErr, &schemaErr) {
		return false, diagnostic
	}
	if terminalValidationLocationsAdmitted(schemaErr, admitted) {
		return true, ""
	}
	return false, diagnostic
}

func safeSchemaDiagnostic(
	factory string,
	source capability.SecretDeclarationSource,
	validationErr error,
) string {
	var schemaErr *jsonschema.ValidationError
	if !errors.As(validationErr, &schemaErr) {
		return ""
	}
	leaf := firstValidationLeaf(schemaErr)
	if leaf == nil {
		return ""
	}
	location := leaf.InstanceLocation
	if location == "" {
		location = "/"
	}
	message := safeValidationMessage(leaf)
	if message == "" {
		return ""
	}
	label := "plugin " + factory + " config"
	switch source {
	case capability.SecretPluginMetadata:
		label = "plugin " + factory + " metadata"
	case capability.SecretConsumerConfig:
		label = "consumer plugin " + factory + " config"
	}
	return fmt.Sprintf("validate %s: %s: %s", label, location, message)
}

func firstValidationLeaf(root *jsonschema.ValidationError) *jsonschema.ValidationError {
	if root == nil {
		return nil
	}
	leaves := make([]*jsonschema.ValidationError, 0, 1)
	var collect func(*jsonschema.ValidationError)
	collect = func(current *jsonschema.ValidationError) {
		if len(current.Causes) == 0 {
			leaves = append(leaves, current)
			return
		}
		for _, cause := range current.Causes {
			collect(cause)
		}
	}
	collect(root)
	slices.SortFunc(leaves, func(left, right *jsonschema.ValidationError) int {
		if byLocation := strings.Compare(left.InstanceLocation, right.InstanceLocation); byLocation != 0 {
			return byLocation
		}
		return strings.Compare(left.KeywordLocation, right.KeywordLocation)
	})
	return leaves[0]
}

func safeValidationMessage(validationErr *jsonschema.ValidationError) string {
	keyword := validationErr.KeywordLocation
	if index := strings.LastIndexByte(keyword, '/'); index >= 0 {
		keyword = keyword[index+1:]
	}
	switch keyword {
	case "type", "required", "additionalProperties", "dependentRequired", "dependencies":
		return validationErr.Message
	case "enum":
		return "value is not in the allowed set"
	case "const":
		return "value does not match the required constant"
	case "format":
		return "value does not match the required format"
	case "pattern":
		return validationErr.Message
	case "minimum", "exclusiveMinimum", "minLength", "minItems", "minProperties":
		return schemaConstraintWithoutInstanceValue(validationErr.Message)
	case "maximum", "exclusiveMaximum", "maxLength", "maxItems", "maxProperties":
		return schemaConstraintWithoutInstanceValue(validationErr.Message)
	case "multipleOf":
		return "value is not an allowed multiple"
	case "uniqueItems":
		return "array items must be unique"
	default:
		return "value does not satisfy schema rule " + keyword
	}
}

func schemaConstraintWithoutInstanceValue(message string) string {
	for _, separator := range []string{", but got ", ", but found ", " but found "} {
		if prefix, _, found := strings.Cut(message, separator); found {
			return prefix
		}
	}
	return message
}

func terminalValidationLocationsAdmitted(
	validationErr *jsonschema.ValidationError,
	admitted map[string]struct{},
) bool {
	if validationErr == nil {
		return false
	}
	if len(validationErr.Causes) == 0 {
		_, ok := admitted[validationErr.InstanceLocation]
		return ok
	}
	for _, cause := range validationErr.Causes {
		if !terminalValidationLocationsAdmitted(cause, admitted) {
			return false
		}
	}
	return true
}
