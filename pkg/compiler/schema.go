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
		valid := exists && entry.metadata != nil && schemaAccepts(
			entry.metadata,
			schemas.catalog,
			resource.key.ID,
			capability.SecretPluginMetadata,
			resource.document,
		)
		if !valid {
			*issues = append(*issues, newIssue(
				resource.key, pluginMetadataSchemaInvalidCode, "plugin metadata schema validation failed",
			))
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
			if consumerSchema == nil ||
				!schemaAccepts(consumerSchema, schemas.catalog, factory, capability.SecretConsumerConfig, config) {
				*issues = append(*issues, newIssue(
					resource.key, consumerSchemaInvalidCode, "consumer schema validation failed",
				))
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
			if schemaAccepts(entry.config, schemas.catalog, factory, capability.SecretPluginConfig, config) {
				continue
			}
			issue := newIssue(resource.key, pluginSchemaInvalidCode, "plugin schema validation failed")
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

func schemaAccepts(
	compiled *util.CompiledSchema,
	catalog *capability.SecretDeclarationCatalog,
	factory string,
	source capability.SecretDeclarationSource,
	document any,
) bool {
	if compiled == nil {
		return false
	}
	validationErr := compiled.Validate(document)
	if validationErr == nil {
		return true
	}

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
		return false
	}

	var schemaErr *jsonschema.ValidationError
	if !errors.As(validationErr, &schemaErr) {
		return false
	}
	return terminalValidationLocationsAdmitted(schemaErr, admitted)
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
