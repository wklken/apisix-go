package base

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// SecretMaterializer resolves generation-owned credentials after schema
// decoding and before PostInit.
type SecretMaterializer interface {
	MaterializeSecrets() error
}

// MaterializePluginSecrets runs the pre-PostInit secret phase. Plugins that
// expose a secret reference in Config must declare ownership by implementing
// SecretMaterializer.
func MaterializePluginSecrets(p any) error {
	materializer, ownsSecrets := p.(SecretMaterializer)
	if !ownsSecrets {
		if owner, ok := p.(configOwner); ok {
			result := firstUnmaterializedSecretReference(owner.Config())
			if err := secretScanError(result, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := materializer.MaterializeSecrets(); err != nil {
		return redactedSecretMaterializationError{}
	}
	if owner, ok := p.(configOwner); ok {
		result := firstUnmaterializedSecretReference(owner.Config())
		if err := secretScanError(result, true); err != nil {
			return err
		}
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
