package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
)

const encryptedValuePrefix = "$encrypted://"

const (
	v2CiphertextPrefix = "v2:"
	v2NonceSize        = 12
)

func EncryptPluginMetadata(
	name string,
	metadata map[string]any,
	keyring []string,
	catalog *capability.SecretDeclarationCatalog,
) error {
	if catalog == nil {
		return ErrDeclarationCatalogUnavailable
	}
	if len(keyring) == 0 {
		return ErrKeyUnavailable
	}
	return catalog.TransformDeclaredFields(
		name,
		capability.SecretPluginMetadata,
		metadata,
		func(declaration capability.SecretDeclaration, _ string, value any) (any, error) {
			encrypted, err := encryptLeaf(value, keyring, name+"."+declaration.Field)
			if err != nil {
				return value, fmt.Errorf("%s.%s: %w", name, declaration.Field, err)
			}
			return encrypted, nil
		},
	)
}

func EncryptPluginConfigs(
	configs map[string]any,
	keyring []string,
	catalog *capability.SecretDeclarationCatalog,
) error {
	if catalog == nil {
		return ErrDeclarationCatalogUnavailable
	}
	if len(keyring) == 0 {
		return ErrKeyUnavailable
	}
	for name, config := range configs {
		pluginConfig, ok := config.(map[string]any)
		if !ok {
			continue
		}
		if err := catalog.TransformDeclaredFields(
			name,
			capability.SecretPluginConfig,
			pluginConfig,
			func(declaration capability.SecretDeclaration, _ string, value any) (any, error) {
				encrypted, err := encryptLeaf(value, keyring, name+"."+declaration.Field)
				if err != nil {
					return value, fmt.Errorf("%s.%s: %w", name, declaration.Field, err)
				}
				return encrypted, nil
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func encryptLeaf(value any, keyring []string, context string) (any, error) {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return typed, nil
		}
		ciphertext, prefixed := strings.CutPrefix(typed, encryptedValuePrefix)
		if prefixed {
			if ciphertext == "" {
				return nil, ErrInvalidCiphertext
			}
			if strings.HasPrefix(ciphertext, v2CiphertextPrefix) {
				if _, err := decryptEncoded(ciphertext, keyring, context); err != nil {
					return nil, fmt.Errorf("%w: %v", ErrInvalidCiphertext, err)
				}
				return typed, nil
			}
			plaintext, err := decryptCBC(ciphertext, keyring)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidCiphertext, err)
			}
			ciphertext, err = EncryptForContext(plaintext, keyring[0], context)
			if err != nil {
				return nil, err
			}
			return encryptedValuePrefix + ciphertext, nil
		}
		ciphertext, err := EncryptForContext(typed, keyring[0], context)
		if err != nil {
			return nil, err
		}
		return encryptedValuePrefix + ciphertext, nil
	}
	return value, nil
}

func DecryptPluginConfigs(
	configs map[string]any,
	keyring []string,
	catalog *capability.SecretDeclarationCatalog,
) {
	if catalog == nil {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if len(keyring) == 0 {
		return
	}
	decryptPluginConfigsWithResolver(configs, NewResolver(true, keyring), catalog)
}

func decryptPluginConfigsWithResolver(
	configs map[string]any,
	resolver Resolver,
	catalog *capability.SecretDeclarationCatalog,
) {
	for name, config := range configs {
		pluginConfig, ok := config.(map[string]any)
		if !ok {
			continue
		}
		_ = catalog.TransformDeclaredFields(
			name,
			capability.SecretPluginConfig,
			pluginConfig,
			func(declaration capability.SecretDeclaration, _ string, value any) (any, error) {
				return decryptLeaf(value, resolver, name+"."+declaration.Field), nil
			},
		)
	}
}

// DecryptPluginConfigWithResolver decrypts the registered encrypted fields of
// a single plugin configuration. Callers that iterate a resource's plugin map
// reuse one resolver across plugins instead of copying the keyring per call.
func DecryptPluginConfigWithResolver(
	config any,
	pluginName string,
	resolver Resolver,
	catalog *capability.SecretDeclarationCatalog,
) {
	if catalog == nil {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if !resolver.Configured() {
		panic(ErrDeclarationCatalogUnavailable)
	}
	pluginConfig, ok := config.(map[string]any)
	if !ok {
		return
	}
	_ = catalog.TransformDeclaredFields(
		pluginName,
		capability.SecretPluginConfig,
		pluginConfig,
		func(declaration capability.SecretDeclaration, _ string, value any) (any, error) {
			return decryptLeaf(value, resolver, pluginName+"."+declaration.Field), nil
		},
	)
}

func DecryptPluginMetadata(
	name string,
	metadata map[string]any,
	keyring []string,
	catalog *capability.SecretDeclarationCatalog,
) {
	if catalog == nil {
		panic(ErrDeclarationCatalogUnavailable)
	}
	if len(keyring) == 0 {
		return
	}
	resolver := NewResolver(true, keyring)
	_ = catalog.TransformDeclaredFields(
		name,
		capability.SecretPluginMetadata,
		metadata,
		func(declaration capability.SecretDeclaration, _ string, value any) (any, error) {
			return decryptLeaf(value, resolver, name+"."+declaration.Field), nil
		},
	)
}

func decryptLeaf(value any, resolver Resolver, context string) any {
	switch typed := value.(type) {
	case string:
		return resolver.ResolveOptionalForContext(typed, context)
	}
	return value
}

// Decrypt reads APISIX CBC values and values produced by Encrypt. Registered
// field-bound v2 values must use
// Resolver.ResolveForContext so their AAD cannot be bypassed.
func Decrypt(encoded string, keyring []string) (string, error) {
	return decryptEncoded(encoded, keyring, "")
}

func decryptEncoded(encoded string, keyring []string, context string) (string, error) {
	encoded = strings.TrimPrefix(encoded, encryptedValuePrefix)
	if strings.HasPrefix(encoded, v2CiphertextPrefix) {
		return decryptV2(encoded, keyring, context)
	}
	return decryptCBC(encoded, keyring)
}

func decryptV2(encoded string, keyring []string, context string) (string, error) {
	encoded = strings.TrimPrefix(encoded, v2CiphertextPrefix)
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	if len(ciphertext) <= v2NonceSize {
		return "", fmt.Errorf("invalid v2 envelope")
	}
	for _, key := range keyring {
		if len(key) != aes.BlockSize {
			continue
		}
		block, err := aes.NewCipher([]byte(key))
		if err != nil {
			continue
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil || gcm.NonceSize() != v2NonceSize || len(ciphertext) <= gcm.Overhead() {
			continue
		}
		plaintext, err := gcm.Open(nil, ciphertext[:v2NonceSize], ciphertext[v2NonceSize:], []byte(context))
		if err == nil {
			return string(plaintext), nil
		}
	}
	return "", fmt.Errorf("decrypt data encryption field")
}

func decryptCBC(encoded string, keyring []string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	for _, key := range keyring {
		if len(key) != aes.BlockSize || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
			continue
		}
		block, err := aes.NewCipher([]byte(key))
		if err != nil {
			continue
		}
		plaintext := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, []byte(key)).CryptBlocks(plaintext, ciphertext)
		plaintext, err = unpad(plaintext)
		if err == nil {
			return string(plaintext), nil
		}
	}
	return "", fmt.Errorf("decrypt data encryption field")
}

func Encrypt(plaintext string, key string) (string, error) {
	return EncryptForContext(plaintext, key, "")
}

// EncryptForContext seals a value in the versioned data-encryption envelope.
// The canonical field context is authenticated as AEAD associated data.
func EncryptForContext(plaintext string, key string, context string) (string, error) {
	if len(key) != aes.BlockSize {
		return "", errors.New("data encryption key must be 16 bytes")
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || gcm.NonceSize() != v2NonceSize {
		return "", errors.New("data encryption gcm unavailable")
	}
	nonce := make([]byte, v2NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate data encryption nonce: %w", err)
	}
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), []byte(context))
	envelope := append(nonce, sealed...)
	return v2CiphertextPrefix + base64.StdEncoding.EncodeToString(envelope), nil
}

func unpad(value []byte) ([]byte, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("invalid padding")
	}
	padding := int(value[len(value)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(value) {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, b := range value[len(value)-padding:] {
		if int(b) != padding {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return value[:len(value)-padding], nil
}
