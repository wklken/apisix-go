package base

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
)

// ScopedSecretAccess binds every generation/resource dimension except the
// declared field. Plugins can select only a field for their own factory.
type ScopedSecretAccess struct {
	scope   secret.Scope
	secrets secret.GenerationSecrets
}

func (access ScopedSecretAccess) Materialize(
	ctx context.Context,
	field string,
	raw string,
) (secret.Value, error) {
	scope := access.scope
	scope.Field = field
	return access.secrets.Materialize(ctx, scope, raw)
}

// Child derives access for one composite-owned child factory without changing
// any other authority dimension.
func (access ScopedSecretAccess) Child(factory string) (ScopedSecretAccess, error) {
	if factory == "" {
		return ScopedSecretAccess{}, secret.ErrInvalidScope
	}
	child := access
	child.scope.Plugin = factory
	return child, nil
}

// ValidFor reports whether access and expected belong to the same generation.
func (access ScopedSecretAccess) ValidFor(expected secret.GenerationSecrets) bool {
	if !expected.Valid() || validateScopedSecretScope(access.scope, access.secrets) != nil {
		return false
	}
	return access.secrets.SameGeneration(expected)
}

// SharedLimiter returns a generation-owned limiter after validating the same
// scope used for secret materialization.
func (access ScopedSecretAccess) SharedLimiter(name string, capacity int) (secret.GenerationLimiter, error) {
	if err := validateScopedSecretScope(access.scope, access.secrets); err != nil {
		return secret.GenerationLimiter{}, err
	}
	return access.secrets.SharedLimiter(name, capacity)
}

type ScopedSecretMaterializer interface {
	MaterializeScopedSecrets(context.Context, ScopedSecretAccess) error
}

// MaterializeScopedPluginSecrets runs the generation-bound secret phase.
func MaterializeScopedPluginSecrets(
	ctx context.Context,
	baseScope secret.Scope,
	secrets secret.GenerationSecrets,
	p any,
) error {
	if err := validateScopedSecretScope(baseScope, secrets); err != nil {
		return err
	}
	return materializeScopedPluginSecrets(
		ctx,
		ScopedSecretAccess{scope: baseScope, secrets: secrets},
		p,
	)
}

// MaterializeScopedCompositeChildSecrets runs the same scoped-only body for a
// child whose authority was derived with ScopedSecretAccess.Child. It never
// crosses into the transitional process-global materializer.
func MaterializeScopedCompositeChildSecrets(
	ctx context.Context,
	access ScopedSecretAccess,
	p any,
) error {
	if err := validateScopedSecretScope(access.scope, access.secrets); err != nil {
		return err
	}
	return materializeScopedPluginSecrets(ctx, access, p)
}

func materializeScopedPluginSecrets(
	ctx context.Context,
	access ScopedSecretAccess,
	p any,
) error {
	materializer, ownsSecrets := p.(ScopedSecretMaterializer)
	if !ownsSecrets {
		if owner, ok := p.(configOwner); ok {
			return secretScanError(firstUnmaterializedSecretReference(owner.Config()), false)
		}
		return nil
	}
	if err := materializer.MaterializeScopedSecrets(ctx, access); err != nil {
		if ctx != nil {
			if contextErr := ctx.Err(); errors.Is(contextErr, context.Canceled) {
				return context.Canceled
			}
			if contextErr := ctx.Err(); errors.Is(contextErr, context.DeadlineExceeded) {
				return context.DeadlineExceeded
			}
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return redactedSecretMaterializationError{}
	}
	if owner, ok := p.(configOwner); ok {
		return secretScanError(firstUnmaterializedSecretReference(owner.Config()), true)
	}
	return nil
}

func validateScopedSecretScope(
	scope secret.Scope,
	secrets secret.GenerationSecrets,
) error {
	if !secrets.Valid() {
		return secret.ErrInvalidCapability
	}
	if scope.Generation != secrets.Generation() {
		return secret.ErrCapabilityScopeMismatch
	}
	if (scope.Domain != generation.DomainHTTP && scope.Domain != generation.DomainStream) ||
		scope.Plugin == "" || scope.Resource.Kind == "" || scope.Resource.ID == "" ||
		(scope.Source != capability.SecretPluginConfig &&
			scope.Source != capability.SecretConsumerConfig) || scope.Field != "" {
		return secret.ErrInvalidScope
	}
	return nil
}

type configOwner interface {
	Config() any
}

type redactedSecretMaterializationError struct{}

func (e redactedSecretMaterializationError) Error() string {
	return "materialize plugin secrets: credential unavailable"
}

func (e redactedSecretMaterializationError) Is(target error) bool {
	return target != nil && target.Error() == "credential unavailable"
}

type secretScanStatus uint8

const (
	secretScanClean secretScanStatus = iota
	secretScanFound
	secretScanDepthExceeded
)

const maxSecretScanDepth = 32

type secretScanResult struct {
	path   string
	status secretScanStatus
}

func firstUnmaterializedSecretReference(config any) secretScanResult {
	return firstSecretReference(reflect.ValueOf(config), "", 0, make(map[uintptr]struct{}))
}

func firstSecretReference(value reflect.Value, path string, depth int, visited map[uintptr]struct{}) secretScanResult {
	if !value.IsValid() {
		return secretScanResult{status: secretScanClean}
	}
	if depth >= maxSecretScanDepth {
		return secretScanResult{path: path, status: secretScanDepthExceeded}
	}

	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return secretScanResult{status: secretScanClean}
		}
		value = value.Elem()
	}

	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return secretScanResult{status: secretScanClean}
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return secretScanResult{status: secretScanClean}
		}
		visited[pointer] = struct{}{}
		defer delete(visited, pointer)
		return firstSecretReference(value.Elem(), path, depth+1, visited)
	case reflect.String:
		if isUnmaterializedSecretReference(value.String()) {
			return secretScanResult{path: path, status: secretScanFound}
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for i := 0; i < value.NumField(); i++ {
			fieldPath := appendSecretPath(path, secretFieldName(typeOfValue.Field(i)))
			result := firstSecretReference(value.Field(i), fieldPath, depth+1, visited)
			if result.status != secretScanClean {
				return result
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return secretScanResult{status: secretScanClean}
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for index, key := range keys {
			keyPath := fmt.Sprintf("%s[%d]", path, index)
			result := firstSecretReference(value.MapIndex(key), keyPath, depth+1, visited)
			if result.status != secretScanClean {
				return result
			}
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			indexPath := fmt.Sprintf("%s[%d]", path, i)
			result := firstSecretReference(value.Index(i), indexPath, depth+1, visited)
			if result.status != secretScanClean {
				return result
			}
		}
	}

	return secretScanResult{status: secretScanClean}
}

func secretScanError(result secretScanResult, owned bool) error {
	switch result.status {
	case secretScanFound:
		if owned {
			return fmt.Errorf("secret reference remains unmaterialized at %s", result.path)
		}
		return fmt.Errorf("unowned secret reference at %s", result.path)
	case secretScanDepthExceeded:
		return fmt.Errorf("secret reference scan depth exceeded at %s", boundedSecretScanPath(result.path))
	default:
		return nil
	}
}

func boundedSecretScanPath(path string) string {
	if path == "" {
		return "<root>"
	}
	return path
}

func isUnmaterializedSecretReference(value string) bool {
	if len(value) >= len("$ENV://") && strings.EqualFold(value[:len("$ENV://")], "$ENV://") {
		return !strings.Contains(value, "#sha256:")
	}
	return strings.HasPrefix(value, "$secret://") && !strings.Contains(value, "#sha256:")
}

func appendSecretPath(path, segment string) string {
	if path == "" {
		return segment
	}
	return path + "." + segment
}

func secretFieldName(field reflect.StructField) string {
	name := strings.Split(field.Tag.Get("json"), ",")[0]
	if name == "" || name == "-" {
		return field.Name
	}
	return name
}
