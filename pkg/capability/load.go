package capability

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

const (
	expectedTargetName    = "apisix-3.17"
	expectedTargetVersion = "3.17.0"
	expectedSourceCommit  = "9ef2ecab67f652d38365049613610ef649bb4ad0"
	expectedTargetImage   = "apache/apisix:3.17.0"
)

//go:embed manifest.yaml
var manifestYAML []byte

func Load() (*Manifest, error) { return Parse(manifestYAML) }

func Parse(data []byte) (*Manifest, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode manifest: multiple YAML documents are not supported")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	return &manifest, nil
}

func (m *Manifest) Plugin(name string) (PluginCapability, bool) {
	if m == nil {
		return PluginCapability{}, false
	}
	index, ok := m.pluginsByName[name]
	if !ok {
		return PluginCapability{}, false
	}
	if index < 0 || index >= len(m.Plugins) {
		return PluginCapability{}, false
	}
	return clonePlugin(m.Plugins[index]), true
}

func (m *Manifest) validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("schema_version %d is unsupported; want 1", m.SchemaVersion)
	}
	if m.Target.Name != expectedTargetName ||
		m.Target.Version != expectedTargetVersion ||
		m.Target.SourceCommit != expectedSourceCommit ||
		m.Target.Image != expectedTargetImage {
		return fmt.Errorf(
			"target must be %s %s at %s with image %s",
			expectedTargetName, expectedTargetVersion, expectedSourceCommit, expectedTargetImage,
		)
	}

	m.pluginsByName = make(map[string]int, len(m.Plugins))
	factoryByKey := make(map[string]int)
	for index, plugin := range m.Plugins {
		if strings.TrimSpace(plugin.Name) == "" {
			return fmt.Errorf("plugins[%d]: name must not be blank", index)
		}
		if _, exists := m.pluginsByName[plugin.Name]; exists {
			return fmt.Errorf("duplicate plugin id %q", plugin.Name)
		}
		m.pluginsByName[plugin.Name] = index
		if err := validatePlugin(plugin); err != nil {
			return fmt.Errorf("plugin %q: %w", plugin.Name, err)
		}
		for _, factory := range plugin.Factories {
			if _, exists := factoryByKey[factory.Key]; exists {
				return fmt.Errorf("duplicate factory id %q", factory.Key)
			}
			if other, exists := m.pluginsByName[factory.Key]; exists && other != index {
				return fmt.Errorf("factory id %q collides with plugin id", factory.Key)
			}
			factoryByKey[factory.Key] = index
			m.pluginsByName[factory.Key] = index
		}
	}

	if _, err := NewSecretDeclarationCatalog(m); err != nil {
		return fmt.Errorf("secret declarations: %w", err)
	}
	return nil
}

func NewSecretDeclarationCatalog(manifest *Manifest) (*SecretDeclarationCatalog, error) {
	if manifest == nil {
		return nil, errors.New("manifest must not be nil")
	}

	declarations := make([]SecretDeclaration, 0)
	owners := make(map[string]string)
	lookup := make(map[secretDeclarationKey]SecretDeclaration)
	for _, plugin := range manifest.Plugins {
		factories := make(map[string]struct{}, len(plugin.Factories))
		for factoryIndex, factory := range plugin.Factories {
			if strings.TrimSpace(factory.Key) == "" {
				return nil, fmt.Errorf("plugin %q factory %d: key must not be blank", plugin.Name, factoryIndex)
			}
			if previous, exists := owners[factory.Key]; exists {
				return nil, fmt.Errorf("factory %q is owned by both %q and %q", factory.Key, previous, plugin.Name)
			}
			owners[factory.Key] = plugin.Name
			factories[factory.Key] = struct{}{}
		}
		for declarationIndex, declaration := range plugin.SecretDeclarations {
			if _, owned := factories[declaration.Factory]; !owned {
				return nil, fmt.Errorf(
					"plugin %q declaration %d: factory %q is not owned by the capability",
					plugin.Name,
					declarationIndex,
					declaration.Factory,
				)
			}
			if !validSecretDeclarationSource(declaration.Source) {
				return nil, fmt.Errorf(
					"plugin %q declaration %d: unknown source %q",
					plugin.Name,
					declarationIndex,
					declaration.Source,
				)
			}
			if !canonicalSecretFieldPath(declaration.Field) {
				return nil, fmt.Errorf(
					"plugin %q declaration %d: field %q is not a canonical wildcard path",
					plugin.Name,
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
						"plugin %q declaration %d: duplicate factory/source/field tuple",
						plugin.Name,
						declarationIndex,
					)
				}
				return nil, fmt.Errorf(
					"plugin %q declaration %d: field %q overlaps declared field %q",
					plugin.Name,
					declarationIndex,
					declaration.Field,
					previous.Field,
				)
			}
			lookup[key] = declaration
			declarations = append(declarations, declaration)
		}
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

func validatePlugin(plugin PluginCapability) error {
	if !validNamespace(plugin.Namespace) {
		return fmt.Errorf("unknown namespace %q", plugin.Namespace)
	}
	if plugin.Namespace == NamespaceAPISIX && len(plugin.Domains) == 0 {
		return errors.New("apisix plugin must declare a domain")
	}
	seenDomains := make(map[Domain]struct{}, len(plugin.Domains))
	for _, domain := range plugin.Domains {
		if !validDomain(domain) {
			return fmt.Errorf("unknown domain %q", domain)
		}
		if _, exists := seenDomains[domain]; exists {
			return fmt.Errorf("duplicate domain %q", domain)
		}
		seenDomains[domain] = struct{}{}
	}

	for _, factory := range plugin.Factories {
		if strings.TrimSpace(factory.Key) == "" {
			return errors.New("factory key must not be blank")
		}
		if strings.TrimSpace(factory.ImportPath) == "" ||
			strings.TrimSpace(factory.ImportAlias) == "" ||
			strings.TrimSpace(factory.Constructor) == "" {
			return fmt.Errorf("factory %q requires import_path, import_alias, and constructor", factory.Key)
		}
	}
	return nil
}

func validNamespace(namespace Namespace) bool {
	return namespace == NamespaceAPISIX || namespace == NamespaceGoV1
}

func validDomain(domain Domain) bool {
	return domain == DomainHTTP || domain == DomainStream
}

func clonePlugin(plugin PluginCapability) PluginCapability {
	plugin.Domains = append([]Domain(nil), plugin.Domains...)
	plugin.Factories = append([]Factory(nil), plugin.Factories...)
	plugin.Phases = append([]string(nil), plugin.Phases...)
	plugin.Scopes = append([]string(nil), plugin.Scopes...)
	if plugin.SecretDeclarations != nil {
		plugin.SecretDeclarations = append([]SecretDeclaration{}, plugin.SecretDeclarations...)
	}
	return plugin
}
