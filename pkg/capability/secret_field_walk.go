package capability

import (
	"sort"
	"strconv"
	"strings"
)

const (
	managedSecretPrefix     = "$secret://"
	environmentSecretPrefix = "$ENV://"
	encryptedSecretPrefix   = "$encrypted://"
)

// SecretFieldTransform transforms one leaf selected by a secret declaration.
// pointer is the concrete RFC 6901 JSON pointer to that leaf.
type SecretFieldTransform func(
	declaration SecretDeclaration,
	pointer string,
	value any,
) (replacement any, err error)

// TransformDeclaredFields visits and replaces leaves selected by declarations
// for one factory and source. Missing paths and shape mismatches are ignored.
func (c *SecretDeclarationCatalog) TransformDeclaredFields(
	factory string,
	source SecretDeclarationSource,
	document any,
	transform SecretFieldTransform,
) error {
	if c == nil || transform == nil {
		return nil
	}

	var transformErr error
	c.ForEach(factory, source, func(declaration SecretDeclaration) {
		if transformErr != nil {
			return
		}
		transformErr = transformDeclaredPath(
			document,
			strings.Split(declaration.Field, "."),
			"",
			declaration,
			transform,
		)
	})
	return transformErr
}

func transformDeclaredPath(
	current any,
	segments []string,
	pointer string,
	declaration SecretDeclaration,
	transform SecretFieldTransform,
) error {
	if len(segments) == 0 {
		return nil
	}

	segment := segments[0]
	switch value := current.(type) {
	case map[string]any:
		keys := matchingDeclaredKeys(value, segment)
		for _, key := range keys {
			child := value[key]
			childPointer := appendJSONPointer(pointer, key)
			if len(segments) == 1 {
				replacement, err := transformDeclaredLeaves(child, childPointer, declaration, transform)
				if err != nil {
					return err
				}
				value[key] = replacement
				continue
			}
			if err := transformDeclaredPath(child, segments[1:], childPointer, declaration, transform); err != nil {
				return err
			}
		}
	case []any:
		if segment != "*" {
			return nil
		}
		for index, child := range value {
			childPointer := appendJSONPointer(pointer, strconv.Itoa(index))
			if len(segments) == 1 {
				replacement, err := transformDeclaredLeaves(child, childPointer, declaration, transform)
				if err != nil {
					return err
				}
				value[index] = replacement
				continue
			}
			if err := transformDeclaredPath(child, segments[1:], childPointer, declaration, transform); err != nil {
				return err
			}
		}
	}
	return nil
}

func transformDeclaredLeaves(
	value any,
	pointer string,
	declaration SecretDeclaration,
	transform SecretFieldTransform,
) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		keys := sortedMapKeys(typed)
		for _, key := range keys {
			replacement, err := transformDeclaredLeaves(
				typed[key],
				appendJSONPointer(pointer, key),
				declaration,
				transform,
			)
			if err != nil {
				return value, err
			}
			typed[key] = replacement
		}
		return typed, nil
	case []any:
		for index, child := range typed {
			replacement, err := transformDeclaredLeaves(
				child,
				appendJSONPointer(pointer, strconv.Itoa(index)),
				declaration,
				transform,
			)
			if err != nil {
				return value, err
			}
			typed[index] = replacement
		}
		return typed, nil
	default:
		return transform(declaration, pointer, value)
	}
}

func matchingDeclaredKeys(value map[string]any, segment string) []string {
	if segment == "*" {
		return sortedMapKeys(value)
	}
	keys := make([]string, 0, 1)
	for key := range value {
		if strings.EqualFold(key, segment) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func appendJSONPointer(pointer string, segment string) string {
	segment = strings.ReplaceAll(segment, "~", "~0")
	segment = strings.ReplaceAll(segment, "/", "~1")
	return pointer + "/" + segment
}

// IsMaterializableSecretEnvelope reports whether value has a supported raw
// secret envelope prefix and a non-empty payload. It does not validate the
// referenced secret or encrypted ciphertext.
func IsMaterializableSecretEnvelope(value string) bool {
	if strings.HasPrefix(value, managedSecretPrefix) {
		return len(value) > len(managedSecretPrefix)
	}
	if len(value) >= len(environmentSecretPrefix) &&
		strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix) {
		return len(value) > len(environmentSecretPrefix)
	}
	if strings.HasPrefix(value, encryptedSecretPrefix) {
		return len(value) > len(encryptedSecretPrefix)
	}
	return false
}
