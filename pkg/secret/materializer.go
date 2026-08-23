package secret

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

var (
	ErrCredentialUnavailable     = errors.New("credential unavailable")
	ErrInvalidCapability         = errors.New("invalid secret capability")
	ErrCapabilityScopeMismatch   = errors.New("secret capability scope mismatch")
	ErrInvalidScope              = errors.New("invalid secret scope")
	errSecretUseCallbackRequired = errors.New("secret use callback is required")
)

type Scope struct {
	Generation uint64
	Plugin     string
	Resource   generation.ResourceKey
	Field      string // Canonical registered at-rest field path, including any wildcard segment.
}

type Value struct {
	plaintext string
	digest    [32]byte
}

func (value Value) Use(use func(string) error) error {
	if use == nil {
		return errSecretUseCallbackRequired
	}
	return use(value.plaintext)
}

func (value Value) Digest() [32]byte {
	return value.digest
}

type ReferenceResolver interface {
	Resolve(context.Context, string) (string, error)
}

type ScopedResolver interface {
	ResolveScoped(context.Context, Scope, string) (string, error)
}

type Materializer interface {
	Materialize(context.Context, Scope, string) (Value, error)
}

type materializer struct {
	encryption data_encryption.Service
	references ReferenceResolver
}

func NewMaterializer(encryption data_encryption.Service, references ReferenceResolver) Materializer {
	return materializer{encryption: encryption, references: references}
}

func (materializer materializer) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if err := validateScope(scope); err != nil {
		return Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}

	resolved := raw
	if !isReference(resolved) {
		resolver := materializer.encryption.Resolver()
		materializationContext := scope.Plugin + "." + scope.Field
		if isStrictScope(scope) {
			var err error
			resolved, err = resolver.ResolveForContext(resolved, materializationContext)
			if err != nil {
				return Value{}, ErrCredentialUnavailable
			}
		} else {
			resolved = resolver.ResolveOptionalForContext(resolved, materializationContext)
		}
	}
	if isReference(resolved) {
		if materializer.references == nil {
			return Value{}, ErrCredentialUnavailable
		}
		value, err := materializer.references.Resolve(ctx, resolved)
		if err != nil {
			return Value{}, ErrCredentialUnavailable
		}
		resolved = value
	}
	return newValue(resolved), nil
}

type scopedMaterializer struct {
	resolver ScopedResolver
}

func NewScopedMaterializer(resolver ScopedResolver) Materializer {
	return scopedMaterializer{resolver: resolver}
}

func (materializer scopedMaterializer) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if err := validateScope(scope); err != nil {
		return Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if materializer.resolver == nil {
		return Value{}, ErrCredentialUnavailable
	}

	plaintext, err := materializer.resolver.ResolveScoped(ctx, scope, raw)
	if err != nil {
		return Value{}, ErrCredentialUnavailable
	}
	return newValue(plaintext), nil
}

type GenerationCapability struct {
	generation   uint64
	materializer Materializer
}

func NewGenerationCapability(materializer Materializer, generation uint64) (GenerationCapability, error) {
	if materializer == nil || generation == 0 {
		return GenerationCapability{}, ErrInvalidCapability
	}
	return GenerationCapability{generation: generation, materializer: materializer}, nil
}

func (capability GenerationCapability) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if scope.Generation != capability.generation {
		return Value{}, ErrCapabilityScopeMismatch
	}
	return capability.materializer.Materialize(ctx, scope, raw)
}

func validateScope(scope Scope) error {
	if scope.Generation == 0 || scope.Plugin == "" || scope.Resource.Kind == "" || scope.Resource.ID == "" ||
		scope.Field == "" {
		return ErrInvalidScope
	}
	return nil
}

func newValue(plaintext string) Value {
	return Value{
		plaintext: plaintext,
		digest:    sha256.Sum256([]byte(plaintext)),
	}
}

func isReference(value string) bool {
	return strings.HasPrefix(value, "$secret://") || strings.HasPrefix(strings.ToUpper(value), "$ENV://")
}

func isStrictScope(scope Scope) bool {
	if scope.Resource.Kind == "plugin_metadata" {
		return data_encryption.IsStrictPluginMetadataField(scope.Plugin, scope.Field)
	}
	return data_encryption.IsStrictPluginField(scope.Plugin, scope.Field)
}
