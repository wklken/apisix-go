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
			if path, found := firstUnmaterializedSecretReference(owner.Config()); found {
				return fmt.Errorf("unowned secret reference at %s", path)
			}
		}
		return nil
	}
	if err := materializer.MaterializeSecrets(); err != nil {
		return redactedSecretMaterializationError{}
	}
	if owner, ok := p.(configOwner); ok {
		if path, found := firstUnmaterializedSecretReference(owner.Config()); found {
			return fmt.Errorf("secret reference remains unmaterialized at %s", path)
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

func firstUnmaterializedSecretReference(config any) (string, bool) {
	return firstSecretReference(reflect.ValueOf(config), "", 0, make(map[uintptr]struct{}))
}

func firstSecretReference(value reflect.Value, path string, depth int, visited map[uintptr]struct{}) (string, bool) {
	if !value.IsValid() || depth >= 32 {
		return "", false
	}

	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return "", false
		}
		value = value.Elem()
	}

	switch value.Kind() {
	case reflect.Ptr:
		if value.IsNil() {
			return "", false
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return "", false
		}
		visited[pointer] = struct{}{}
		defer delete(visited, pointer)
		return firstSecretReference(value.Elem(), path, depth+1, visited)
	case reflect.String:
		if isUnmaterializedSecretReference(value.String()) {
			return path, true
		}
	case reflect.Struct:
		typeOfValue := value.Type()
		for i := 0; i < value.NumField(); i++ {
			fieldPath := appendSecretPath(path, secretFieldName(typeOfValue.Field(i)))
			if foundPath, found := firstSecretReference(value.Field(i), fieldPath, depth+1, visited); found {
				return foundPath, true
			}
		}
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return "", false
		}
		keys := value.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for index, key := range keys {
			keyPath := fmt.Sprintf("%s[%d]", path, index)
			if foundPath, found := firstSecretReference(value.MapIndex(key), keyPath, depth+1, visited); found {
				return foundPath, true
			}
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			indexPath := fmt.Sprintf("%s[%d]", path, i)
			if foundPath, found := firstSecretReference(value.Index(i), indexPath, depth+1, visited); found {
				return foundPath, true
			}
		}
	}

	return "", false
}

func isUnmaterializedSecretReference(value string) bool {
	if !strings.HasPrefix(value, "$ENV://") && !strings.HasPrefix(value, "$secret://") {
		return false
	}
	return !strings.Contains(value, "#sha256:")
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
