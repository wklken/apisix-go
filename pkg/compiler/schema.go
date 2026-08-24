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
	config   *util.CompiledSchema
	metadata *util.CompiledSchema
	consumer *util.CompiledSchema
	domains  []generation.Domain
}

type schemaSet struct {
	factories map[string]factorySchemas
	catalog   *capability.SecretDeclarationCatalog
}

func newSchemaSet(manifest *capability.Manifest) (*schemaSet, error) {
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		return nil, fmt.Errorf("secret declaration catalog: %w", err)
	}

	factoryCount := 0
	for _, pluginCapability := range manifest.Plugins {
		factoryCount += len(pluginCapability.Factories)
	}
	set := &schemaSet{
		factories: make(map[string]factorySchemas, factoryCount),
		catalog:   catalog,
	}
	for _, pluginCapability := range manifest.Plugins {
		domains := schemaGenerationDomains(pluginCapability.Domains)
		for _, factory := range pluginCapability.Factories {
			if _, exists := set.factories[factory.Key]; exists {
				return nil, fmt.Errorf("factory %q has duplicate schema ownership", factory.Key)
			}
			witness, err := plugin.SchemaWitnessForFactory(factory.Key)
			if err != nil {
				return nil, fmt.Errorf("factory %q schema witness is unavailable", factory.Key)
			}
			if witness.Factory != factory.Key {
				return nil, fmt.Errorf("factory %q schema witness does not match", factory.Key)
			}
			config, err := compileFactorySchema(factory.Key, "config", witness.Config, true)
			if err != nil {
				return nil, err
			}
			metadata, err := compileFactorySchema(factory.Key, "metadata", witness.Metadata, false)
			if err != nil {
				return nil, err
			}

			consumerWitness, hasConsumerWitness := consumer.SchemaWitnessForFactory(factory.Key)
			if hasConsumerWitness != witness.HasConsumer {
				return nil, fmt.Errorf("factory %q consumer schema capability does not match", factory.Key)
			}
			var consumerSchema *util.CompiledSchema
			if witness.HasConsumer {
				if consumerWitness.Factory != factory.Key {
					return nil, fmt.Errorf("factory %q consumer schema witness does not match", factory.Key)
				}
				consumerSchema, err = compileFactorySchema(factory.Key, "consumer", consumerWitness.Schema, true)
				if err != nil {
					return nil, err
				}
			}
			set.factories[factory.Key] = factorySchemas{
				config: config, metadata: metadata, consumer: consumerSchema, domains: domains,
			}
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

func schemaGenerationDomains(domains []capability.Domain) []generation.Domain {
	result := make([]generation.Domain, 0, len(domains))
	for _, domain := range domains {
		switch domain {
		case capability.DomainHTTP:
			result = append(result, generation.DomainHTTP)
		case capability.DomainStream:
			result = append(result, generation.DomainStream)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
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
			if entry.consumer == nil ||
				!schemaAccepts(entry.consumer, schemas.catalog, factory, capability.SecretConsumerConfig, config) {
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
