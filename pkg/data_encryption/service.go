package data_encryption

import (
	"errors"
	"fmt"

	"github.com/wklken/apisix-go/pkg/capability"
)

var ErrDeclarationCatalogUnavailable = errors.New("secret declaration catalog unavailable")

// Service owns one immutable-by-convention data-encryption configuration.
// It clones key material on construction and never exposes the keyring.
type Service struct {
	enabled bool
	keyring []string
	catalog *capability.SecretDeclarationCatalog
}

func NewService(enabled bool, keyring []string, catalog *capability.SecretDeclarationCatalog) Service {
	if catalog == nil {
		panic(ErrDeclarationCatalogUnavailable)
	}
	return Service{
		enabled: enabled,
		keyring: append([]string(nil), keyring...),
		catalog: catalog,
	}
}

// Configured distinguishes an explicitly disabled service from a zero service
// that has no encrypted-field declaration catalog.
func (s Service) Configured() bool {
	return s.catalog != nil
}

func (s Service) Enabled() bool {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	return s.enabled
}

func (s Service) Resolver() Resolver {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	return newResolverWithKeyring(s.enabled, s.keyring)
}

func (s Service) DeclarationDigest() [32]byte {
	if !s.Configured() {
		return [32]byte{}
	}
	return s.catalog.Digest()
}

func (s Service) ValidateDeclaration(
	factory string,
	source capability.SecretDeclarationSource,
	field string,
) (capability.SecretDeclaration, error) {
	if !s.Configured() {
		return capability.SecretDeclaration{}, ErrDeclarationCatalogUnavailable
	}
	declaration, ok := s.catalog.Lookup(factory, source, field)
	if !ok {
		return capability.SecretDeclaration{}, fmt.Errorf(
			"undeclared secret field %q/%q/%q",
			factory,
			source,
			field,
		)
	}
	return declaration, nil
}

// ResolveDeclared validates the catalog declaration, then follows the
// APISIX encrypt_fields policy: try each key and preserve the original value
// when none can decrypt it. The raw value is intentionally absent from errors
// so undeclared fields cannot leak credentials through diagnostics.
func (s Service) ResolveDeclared(
	factory string,
	source capability.SecretDeclarationSource,
	field string,
	value string,
) (string, error) {
	_, err := s.ValidateDeclaration(factory, source, field)
	if err != nil {
		return "", err
	}
	if !s.enabled {
		return value, nil
	}
	resolver := s.Resolver()
	context := factory + "." + field
	return resolver.ResolveOptionalForContext(value, context), nil
}

func (s Service) EncryptForContext(plaintext string, context string) (string, error) {
	if !s.Configured() {
		return "", ErrDeclarationCatalogUnavailable
	}
	if !s.enabled {
		return plaintext, nil
	}
	if len(s.keyring) == 0 {
		return "", ErrKeyUnavailable
	}
	return EncryptForContext(plaintext, s.keyring[0], context)
}

func (s Service) EncryptPluginConfigs(configs map[string]any) error {
	if !s.Configured() {
		return ErrDeclarationCatalogUnavailable
	}
	if !s.enabled {
		return nil
	}
	return EncryptPluginConfigs(configs, s.keyring, s.catalog)
}

func (s Service) EncryptPluginMetadata(name string, metadata map[string]any) error {
	if !s.Configured() {
		return ErrDeclarationCatalogUnavailable
	}
	if !s.enabled {
		return nil
	}
	return EncryptPluginMetadata(name, metadata, s.keyring, s.catalog)
}

func (s Service) DecryptPluginConfigs(configs map[string]any) {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if !s.enabled {
		return
	}
	decryptPluginConfigsWithResolver(configs, s.Resolver(), s.catalog)
}

func (s Service) DecryptPluginConfig(config any, pluginName string) {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if !s.enabled {
		return
	}
	s.DecryptPluginConfigWithResolver(config, pluginName, s.Resolver())
}

func (s Service) DecryptPluginConfigWithResolver(
	config any,
	pluginName string,
	resolver Resolver,
) {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if !s.enabled {
		return
	}
	DecryptPluginConfigWithResolver(config, pluginName, resolver, s.catalog)
}

func (s Service) DecryptPluginMetadata(name string, metadata map[string]any) {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if !s.enabled {
		return
	}
	DecryptPluginMetadata(name, metadata, s.keyring, s.catalog)
}

func (s Service) HasEncryptedPluginMetadata(name string) bool {
	if !s.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	found := false
	s.catalog.ForEach(name, capability.SecretPluginMetadata, func(capability.SecretDeclaration) {
		found = true
	})
	return found
}
