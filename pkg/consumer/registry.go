package consumer

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/wklken/apisix-go/pkg/util"
)

type definition struct {
	factory    string
	schemaText string
	schema     *util.CompiledSchema
	validate   func(any) error
	lookup     func(any) (string, error)
}

// SchemaWitness is a read-only view of the raw structural schema owned by one
// consumer factory. Resolved validation remains encapsulated by this package.
type SchemaWitness struct {
	Factory string
	Schema  string
}

type keyAuth struct {
	Key string `json:"key"`
}

type basicAuth struct {
	Username string `json:"username"`
}

type jwtAuth struct {
	Key string `json:"key"`
}

type hmacAuth struct {
	KeyID string `json:"key_id"`
}

type ldapAuth struct {
	UserDN string `json:"user_dn"`
}

type jweDecrypt struct {
	Key             any  `json:"key"`
	Secret          any  `json:"secret"`
	IsBase64Encoded bool `json:"is_base64_encoded"`
}

type wolfRBAC struct {
	AppID string `json:"appid"`
}

const keyAuthSchema = `
{
  "type": "object",
  "title": "work with consumer object",
  "required": ["key"],
  "properties": {
    "key": {"type": "string"}
  }
}`

const basicAuthSchema = `
{
  "type": "object",
  "title": "work with consumer object",
  "required": ["username", "password"],
  "properties": {
    "username": {"type": "string"},
    "password": {"type": "string"}
  }
}`

const jwtAuthSchema = `
{
  "type": "object",
  "required": ["key"],
  "properties": {
    "key": {"type": "string", "minLength": 1},
    "secret": {"type": "string", "minLength": 1},
    "public_key": {"type": "string", "minLength": 1},
    "private_key": {"type": "string", "minLength": 1},
    "algorithm": {
      "type": "string",
      "enum": ["HS256", "HS384", "HS512", "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"]
    },
    "exp": {"type": "integer", "minimum": 1},
    "base64_secret": {"type": "boolean"},
    "lifetime_grace_period": {"type": "integer", "minimum": 0}
  }
}`

const hmacAuthSchema = `
{
  "type": "object",
  "title": "work with consumer object",
  "required": ["key_id", "secret_key"],
  "properties": {
    "key_id": {"type": "string", "minLength": 1, "maxLength": 256},
    "secret_key": {"type": "string", "minLength": 1, "maxLength": 256}
  }
}`

const ldapAuthSchema = `
{
  "type": "object",
  "title": "work with consumer object",
  "required": ["user_dn"],
  "properties": {
    "user_dn": {"type": "string"}
  }
}`

const jweDecryptSchema = `
{
  "type": "object",
  "required": ["key", "secret"],
  "properties": {
    "key": {"type": "string"},
    "secret": {"type": "string"},
    "is_base64_encoded": {"type": "boolean"}
  }
}`

const wolfRBACSchema = `
{
  "type": "object",
  "required": ["appid"],
  "properties": {
    "appid": {"type": "string", "minLength": 1},
    "header_prefix": {"type": "string", "minLength": 1},
    "wolf_url": {"type": "string", "minLength": 1}
  }
}`

var definitions = []definition{
	newSchemaDefinition(
		"basic-auth",
		basicAuthSchema,
		lookupParsed(func(config basicAuth) string { return config.Username }),
	),
	newSchemaDefinition(
		"hmac-auth",
		hmacAuthSchema,
		lookupParsed(func(config hmacAuth) string { return config.KeyID }),
	),
	newCustomDefinition(
		"jwe-decrypt",
		jweDecryptSchema,
		func(config any) error {
			var parsed jweDecrypt
			if err := util.Parse(config, &parsed); err != nil {
				return err
			}
			return validateJWEDecrypt(parsed)
		},
		func(config any) (string, error) {
			var parsed jweDecrypt
			if err := util.Parse(config, &parsed); err != nil {
				return "", err
			}
			key, ok := parsed.Key.(string)
			if !ok {
				return "", fmt.Errorf("jwe-decrypt consumer key must be a string")
			}
			return key, nil
		},
	),
	newSchemaDefinition(
		"jwt-auth",
		jwtAuthSchema,
		lookupParsed(func(config jwtAuth) string { return config.Key }),
	),
	newSchemaDefinition(
		"key-auth",
		keyAuthSchema,
		lookupParsed(func(config keyAuth) string { return config.Key }),
	),
	newSchemaDefinition(
		"ldap-auth",
		ldapAuthSchema,
		lookupParsed(func(config ldapAuth) string { return config.UserDN }),
	),
	newSchemaDefinition(
		"wolf-rbac",
		wolfRBACSchema,
		lookupParsed(func(config wolfRBAC) string { return config.AppID }),
	),
}

var definitionsByFactory = func() map[string]*definition {
	indexed := make(map[string]*definition, len(definitions))
	for index := range definitions {
		definition := &definitions[index]
		indexed[definition.factory] = definition
	}
	return indexed
}()

func newSchemaDefinition(
	name string,
	schema string,
	lookup func(any) (string, error),
) definition {
	compiled, err := util.CompileSchema(schema)
	if err != nil {
		panic("compile consumer schema: " + err.Error())
	}
	return definition{
		factory:    name,
		schemaText: schema,
		schema:     compiled,
		lookup:     lookup,
	}
}

func newCustomDefinition(
	name string,
	schema string,
	validate func(any) error,
	lookup func(any) (string, error),
) definition {
	definition := newSchemaDefinition(name, schema, lookup)
	definition.validate = validate
	return definition
}

func lookupParsed[T any](key func(T) string) func(any) (string, error) {
	return func(config any) (string, error) {
		var parsed T
		if err := util.Parse(config, &parsed); err != nil {
			return "", err
		}
		return key(parsed), nil
	}
}

// Factories returns the deterministic registry metadata without exposing the
// registry's backing slice or schema state.
func Factories() []string {
	factories := make([]string, len(definitions))
	for index, definition := range definitions {
		factories[index] = definition.factory
	}
	return factories
}

// Supports reports whether factory has resolved consumer validation and lookup
// behavior in this registry.
func Supports(factory string) bool {
	_, ok := definitionsByFactory[factory]
	return ok
}

// SchemaWitnessForFactory returns the immutable raw schema for factory without
// exposing the compiled validator or the registry's backing definition.
func SchemaWitnessForFactory(factory string) (SchemaWitness, bool) {
	definition, ok := definitionsByFactory[factory]
	if !ok {
		return SchemaWitness{}, false
	}
	return SchemaWitness{Factory: factory, Schema: definition.schemaText}, true
}

// ValidateResolved validates one resolved consumer plugin configuration.
func ValidateResolved(factory string, config any) error {
	definition, ok := definitionsByFactory[factory]
	if !ok {
		return fmt.Errorf("consumer validation is unsupported for plugin %q", factory)
	}
	if definition.validate != nil {
		return definition.validate(config)
	}
	if err := definition.schema.Validate(config); err != nil {
		return fmt.Errorf("%s consumer configuration: %w", factory, err)
	}
	return nil
}

// LookupKey extracts the lookup identity from one resolved consumer plugin
// configuration.
func LookupKey(factory string, config any) (string, error) {
	definition, ok := definitionsByFactory[factory]
	if !ok {
		return "", fmt.Errorf("consumer lookup is unsupported for plugin %q", factory)
	}
	return definition.lookup(config)
}

func validateJWEDecrypt(config jweDecrypt) error {
	if _, ok := config.Key.(string); !ok {
		return fmt.Errorf("jwe-decrypt consumer key must be a string")
	}
	secret, ok := config.Secret.(string)
	if !ok {
		return fmt.Errorf("jwe-decrypt consumer secret must be a string")
	}
	if config.IsBase64Encoded {
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(secret, "="))
		if err != nil {
			decoded, err = base64.StdEncoding.DecodeString(secret)
			if err != nil {
				return fmt.Errorf("jwe-decrypt consumer secret base64 decode: %w", err)
			}
		}
		if len(decoded) != 32 {
			return fmt.Errorf("the secret length after base64 decode should be 32 chars")
		}
		return nil
	}
	if len(secret) != 32 {
		return fmt.Errorf("the secret length should be 32 chars")
	}
	return nil
}
