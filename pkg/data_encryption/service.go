package data_encryption

import "slices"

// Service owns one immutable-by-convention data-encryption configuration.
// It clones key material on construction and never exposes the keyring.
type Service struct {
	enabled bool
	keyring []string
}

func NewService(enabled bool, keyring []string) Service {
	return Service{
		enabled: enabled,
		keyring: append([]string(nil), keyring...),
	}
}

func (s Service) Enabled() bool {
	return s.enabled
}

func (s Service) Resolver() Resolver {
	return NewResolver(s.enabled, s.keyring)
}

// SameConfiguration reports whether two services have identical runtime
// semantics. Keyring order is significant because the first key encrypts new
// values and the complete order controls rotation reads.
func (s Service) SameConfiguration(other Service) bool {
	return s.enabled == other.enabled && slices.Equal(s.keyring, other.keyring)
}

func (s Service) EncryptForContext(plaintext string, context string) (string, error) {
	if !s.enabled {
		return plaintext, nil
	}
	if len(s.keyring) == 0 {
		return "", ErrKeyUnavailable
	}
	return EncryptForContext(plaintext, s.keyring[0], context)
}

func (s Service) EncryptPluginConfigs(configs map[string]any) error {
	if !s.enabled {
		return nil
	}
	return EncryptPluginConfigs(configs, s.keyring)
}

func (s Service) EncryptPluginMetadata(name string, metadata map[string]any) error {
	if !s.enabled {
		return nil
	}
	return EncryptPluginMetadata(name, metadata, s.keyring)
}

func (s Service) DecryptPluginConfigs(configs map[string]any) {
	if !s.enabled {
		return
	}
	DecryptPluginConfigs(configs, s.keyring)
}

func (s Service) DecryptPluginConfig(config any, pluginName string) {
	if !s.enabled {
		return
	}
	DecryptPluginConfigWithResolver(config, pluginName, s.Resolver())
}

func (s Service) DecryptPluginMetadata(name string, metadata map[string]any) {
	if !s.enabled {
		return
	}
	DecryptPluginMetadata(name, metadata, s.keyring)
}
