package capability

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

func NewSecretDeclarationCatalog() (*SecretDeclarationCatalog, error) {
	return newSecretDeclarationCatalog(secretDeclarations)
}

func newSecretDeclarationCatalog(source []SecretDeclaration) (*SecretDeclarationCatalog, error) {
	declarations := make([]SecretDeclaration, 0, len(source))
	lookup := make(map[secretDeclarationKey]SecretDeclaration)
	for declarationIndex, declaration := range source {
		if strings.TrimSpace(declaration.Factory) == "" {
			return nil, fmt.Errorf("declaration %d: factory must not be blank", declarationIndex)
		}
		if !validSecretDeclarationSource(declaration.Source) {
			return nil, fmt.Errorf(
				"declaration %d: unknown source %q",
				declarationIndex,
				declaration.Source,
			)
		}
		if !canonicalSecretFieldPath(declaration.Field) {
			return nil, fmt.Errorf(
				"declaration %d: field %q is not a canonical wildcard path",
				declarationIndex,
				declaration.Field,
			)
		}
		key := secretDeclarationKey{
			factory: declaration.Factory,
			source:  declaration.Source,
			field:   declaration.Field,
		}
		for _, previous := range declarations {
			if previous.Factory != declaration.Factory || previous.Source != declaration.Source ||
				!secretFieldPathsOverlap(previous.Field, declaration.Field) {
				continue
			}
			if strings.EqualFold(previous.Field, declaration.Field) {
				return nil, fmt.Errorf(
					"declaration %d: duplicate factory/source/field tuple",
					declarationIndex,
				)
			}
			return nil, fmt.Errorf(
				"declaration %d: field %q overlaps declared field %q",
				declarationIndex,
				declaration.Field,
				previous.Field,
			)
		}
		lookup[key] = declaration
		declarations = append(declarations, declaration)
	}

	sort.Slice(declarations, func(i, j int) bool {
		left, right := declarations[i], declarations[j]
		if left.Factory != right.Factory {
			return left.Factory < right.Factory
		}
		if left.Source != right.Source {
			return left.Source < right.Source
		}
		return left.Field < right.Field
	})

	canonical := encodeSecretDeclarations(declarations)
	catalog := &SecretDeclarationCatalog{
		declarations: append([]SecretDeclaration(nil), declarations...),
		lookup:       lookup,
		digest:       sha256.Sum256(canonical),
	}
	return catalog, nil
}

func (c *SecretDeclarationCatalog) Declarations() []SecretDeclaration {
	if c == nil {
		return nil
	}
	return append([]SecretDeclaration(nil), c.declarations...)
}

// ForEach visits declarations owned by one factory and source without
// exposing the catalog's backing slice or allocating a filtered copy.
func (c *SecretDeclarationCatalog) ForEach(
	factory string,
	source SecretDeclarationSource,
	visit func(SecretDeclaration),
) {
	if c == nil || visit == nil {
		return
	}
	start := sort.Search(len(c.declarations), func(index int) bool {
		declaration := c.declarations[index]
		if declaration.Factory != factory {
			return declaration.Factory > factory
		}
		return declaration.Source >= source
	})
	for index := start; index < len(c.declarations); index++ {
		declaration := c.declarations[index]
		if declaration.Factory != factory || declaration.Source != source {
			break
		}
		visit(declaration)
	}
}

func (c *SecretDeclarationCatalog) Lookup(
	factory string,
	source SecretDeclarationSource,
	field string,
) (SecretDeclaration, bool) {
	if c == nil {
		return SecretDeclaration{}, false
	}
	declaration, ok := c.lookup[secretDeclarationKey{factory: factory, source: source, field: field}]
	return declaration, ok
}

func (c *SecretDeclarationCatalog) Digest() [32]byte {
	if c == nil {
		return [32]byte{}
	}
	return c.digest
}

type secretDeclarationKey struct {
	factory string
	source  SecretDeclarationSource
	field   string
}

func encodeSecretDeclarations(declarations []SecretDeclaration) []byte {
	var encoded bytes.Buffer
	encoded.WriteString("apisix-go/secret-declarations/v4")
	writeCanonicalUint64(&encoded, uint64(len(declarations)))
	for _, declaration := range declarations {
		writeCanonicalString(&encoded, declaration.Factory)
		writeCanonicalString(&encoded, string(declaration.Source))
		writeCanonicalString(&encoded, declaration.Field)
	}
	return encoded.Bytes()
}

func writeCanonicalString(encoded *bytes.Buffer, value string) {
	writeCanonicalUint64(encoded, uint64(len(value)))
	encoded.WriteString(value)
}

func writeCanonicalUint64(encoded *bytes.Buffer, value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	encoded.Write(buffer[:])
}

func validSecretDeclarationSource(source SecretDeclarationSource) bool {
	return source == SecretPluginConfig || source == SecretPluginMetadata || source == SecretConsumerConfig
}

func canonicalSecretFieldPath(field string) bool {
	if field == "" || strings.TrimSpace(field) != field {
		return false
	}
	segments := strings.Split(field, ".")
	if segments[len(segments)-1] == "*" {
		return false
	}
	for _, segment := range segments {
		if segment == "" {
			return false
		}
		if segment == "*" {
			continue
		}
		for _, char := range segment {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
				(char >= '0' && char <= '9') || char == '_' || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func secretFieldPathsOverlap(left, right string) bool {
	leftSegments := strings.Split(left, ".")
	rightSegments := strings.Split(right, ".")
	limit := min(len(leftSegments), len(rightSegments))
	for index := range limit {
		leftSegment := leftSegments[index]
		rightSegment := rightSegments[index]
		if leftSegment != "*" && rightSegment != "*" && !strings.EqualFold(leftSegment, rightSegment) {
			return false
		}
	}
	return true
}
