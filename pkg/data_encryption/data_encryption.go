package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

const encryptedValuePrefix = "$encrypted://"

const (
	v2CiphertextPrefix = "v2:"
	v2NonceSize        = 12
)

var runtimeConfig struct {
	sync.RWMutex
	enabled bool
	keyring []string
}

func Configure(enabled bool, keyring []string) {
	runtimeConfig.Lock()
	runtimeConfig.enabled = enabled
	runtimeConfig.keyring = append([]string(nil), keyring...)
	runtimeConfig.Unlock()
}

func Keyring() ([]string, bool) {
	runtimeConfig.RLock()
	defer runtimeConfig.RUnlock()
	return append([]string(nil), runtimeConfig.keyring...), runtimeConfig.enabled
}

var pluginFields = map[string][]string{
	"ai-aliyun-content-moderation": {"access_key_secret"},
	"ai-aws-content-moderation":    {"comprehend.secret_access_key"},
	"ai-proxy": {
		"auth.header", "auth.query", "auth.gcp.service_account_json", "auth.aws.secret_access_key",
		"auth.aws.session_token",
	},
	"ai-proxy-multi": {
		"instances.*.auth.header", "instances.*.auth.query", "instances.*.auth.gcp.service_account_json",
		"instances.*.auth.aws.secret_access_key", "instances.*.auth.aws.session_token",
	},
	"ai-rag": {
		"embeddings_provider.azure_openai.api_key", "vector_search_provider.azure_ai_search.api_key",
	},
	"ai-request-rewrite": {
		"auth.header", "auth.query", "auth.gcp.service_account_json", "auth.aws.secret_access_key",
		"auth.aws.session_token",
	},
	"ai-rate-limiting":     {"redis_password", "sentinel_password"},
	"authz-keycloak":       {"client_secret"},
	"authz-casdoor":        {"client_secret", "client_secret_fallbacks"},
	"aws-lambda":           {"authorization.apikey", "authorization.iam.accesskey", "authorization.iam.secretkey"},
	"azure-functions":      {"authorization.apikey"},
	"basic-auth":           {"password"},
	"cas-auth":             {"cookie.secret"},
	"dingtalk-auth":        {"app_secret", "secret"},
	"feishu-auth":          {"app_secret", "secret", "secret_fallbacks"},
	"hmac-auth":            {"secret"},
	"http-logger":          {"auth_header"},
	"jwe-decrypt":          {"key", "secret"},
	"jwt-auth":             {"secret", "private_key"},
	"kafka-logger":         {"brokers.*.sasl_config.password"},
	"kafka-proxy":          {"sasl.password"},
	"key-auth":             {"key"},
	"ldap-auth":            {"user_dn"},
	"openid-connect":       {"client_secret", "client_rsa_private_key", "session.secret", "session.redis.password"},
	"openfunction":         {"authorization.service_token"},
	"openwhisk":            {"service_token"},
	"clickhouse-logger":    {"password"},
	"csrf":                 {"key"},
	"elasticsearch-logger": {"auth.password", "headers.Authorization"},
	"error-log-logger":     {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
	"google-cloud-logging": {"auth_config.private_key"},
	"lago":                 {"token"},
	"loggly":               {"customer_token"},
	"response-rewrite":     {"body", "body_secret"},
	"rocketmq-logger":      {"secret_key"},
	"saml-auth":            {"sp_private_key", "secret"},
	"sls-logger":           {"access_key_secret"},
	"splunk-hec-logging":   {"endpoint.token"},
	"tencent-cloud-cls":    {"secret_key"},
}

var strictPluginFields = map[string][]string{
	"ai-rate-limiting":     {"redis_password", "sentinel_password"},
	"clickhouse-logger":    {"password"},
	"csrf":                 {"key"},
	"elasticsearch-logger": {"auth.password"},
	"error-log-logger":     {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
	"google-cloud-logging": {"auth_config.private_key"},
	"http-logger":          {"auth_header"},
	"kafka-logger":         {"brokers.*.sasl_config.password"},
	"kafka-proxy":          {"sasl.password"},
	"lago":                 {"token"},
	"loggly":               {"customer_token"},
	"rocketmq-logger":      {"secret_key"},
	"response-rewrite":     {"body_secret"},
	"sls-logger":           {"access_key_secret"},
	"splunk-hec-logging":   {"endpoint.token"},
	"tencent-cloud-cls":    {"secret_key"},
}

var pluginMetadataFields = map[string][]string{
	"azure-functions":  {"master_apikey"},
	"error-log-logger": {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
}

var strictPluginMetadataFields = map[string][]string{
	"error-log-logger": {"clickhouse.password", "kafka.brokers.*.sasl_config.password"},
}

func HasEncryptedPluginMetadata(name string) bool {
	return len(pluginMetadataFields[name]) != 0
}

func EncryptPluginMetadata(name string, metadata map[string]any, keyring []string) error {
	if len(keyring) == 0 {
		return ErrKeyUnavailable
	}
	for _, field := range pluginMetadataFields[name] {
		if err := encryptField(metadata, name, field, keyring); err != nil {
			return fmt.Errorf("%s.%s: %w", name, field, err)
		}
	}
	return nil
}

func IsStrictPluginField(pluginName string, field string) bool {
	return slices.Contains(strictPluginFields[pluginName], field)
}

func EncryptPluginConfigs(configs map[string]any, keyring []string) error {
	if len(keyring) == 0 {
		return ErrKeyUnavailable
	}
	for name, fields := range pluginFields {
		config, ok := configs[name].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range fields {
			if err := encryptField(config, name, field, keyring); err != nil {
				return fmt.Errorf("%s.%s: %w", name, field, err)
			}
		}
	}
	return nil
}

func encryptField(config map[string]any, pluginName string, path string, keyring []string) error {
	return encryptPath(config, strings.Split(path, "."), keyring, pluginName+"."+path)
}

func encryptPath(current any, segments []string, keyring []string, context string) error {
	if len(segments) == 0 {
		return nil
	}
	segment := segments[0]
	switch value := current.(type) {
	case map[string]any:
		if segment == "*" {
			for _, child := range value {
				if err := encryptPath(child, segments[1:], keyring, context); err != nil {
					return err
				}
			}
			return nil
		}
		keys := matchingMapKeys(value, segment)
		if len(keys) == 0 {
			return nil
		}
		for _, key := range keys {
			child := value[key]
			if len(segments) == 1 {
				encrypted, err := encryptValue(child, keyring, context)
				if err != nil {
					return err
				}
				value[key] = encrypted
				continue
			}
			if err := encryptPath(child, segments[1:], keyring, context); err != nil {
				return err
			}
		}
		return nil
	case []any:
		if segment != "*" {
			return nil
		}
		for _, child := range value {
			if err := encryptPath(child, segments[1:], keyring, context); err != nil {
				return err
			}
		}
	}
	return nil
}

func encryptValue(value any, keyring []string, context string) (any, error) {
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
			plaintext, err := decryptLegacy(ciphertext, keyring)
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
	case map[string]any:
		for key, child := range typed {
			encrypted, err := encryptValue(child, keyring, context)
			if err != nil {
				return nil, err
			}
			typed[key] = encrypted
		}
	case []any:
		for i, child := range typed {
			encrypted, err := encryptValue(child, keyring, context)
			if err != nil {
				return nil, err
			}
			typed[i] = encrypted
		}
	}
	return value, nil
}

func DecryptPluginConfigs(configs map[string]any, keyring []string) {
	if len(keyring) == 0 {
		return
	}
	resolver := NewResolver(true, keyring)
	for name, config := range configs {
		DecryptPluginConfigWithResolver(config, name, resolver)
	}
}

// DecryptPluginConfigWithResolver decrypts the registered encrypted fields of
// a single plugin configuration. Callers that iterate a resource's plugin map
// reuse one resolver across plugins instead of copying the keyring per call.
func DecryptPluginConfigWithResolver(config any, pluginName string, resolver Resolver) {
	fields, ok := pluginFields[pluginName]
	if !ok {
		return
	}
	pluginConfig, ok := config.(map[string]any)
	if !ok {
		return
	}
	for _, field := range fields {
		if IsStrictPluginField(pluginName, field) {
			continue
		}
		decryptField(pluginConfig, pluginName, field, resolver)
	}
}

func DecryptPluginMetadata(name string, metadata map[string]any, keyring []string) {
	if len(keyring) == 0 {
		return
	}
	resolver := NewResolver(true, keyring)
	for _, field := range pluginMetadataFields[name] {
		if slices.Contains(strictPluginMetadataFields[name], field) {
			continue
		}
		decryptField(metadata, name, field, resolver)
	}
}

func decryptField(config map[string]any, pluginName string, path string, resolver Resolver) {
	decryptPath(config, strings.Split(path, "."), resolver, pluginName+"."+path)
}

func decryptPath(current any, segments []string, resolver Resolver, context string) {
	if len(segments) == 0 {
		return
	}
	segment := segments[0]
	switch value := current.(type) {
	case map[string]any:
		if segment == "*" {
			for _, child := range value {
				decryptPath(child, segments[1:], resolver, context)
			}
			return
		}
		keys := matchingMapKeys(value, segment)
		if len(keys) == 0 {
			return
		}
		for _, key := range keys {
			child := value[key]
			if len(segments) == 1 {
				value[key] = decryptValue(child, resolver, context)
				continue
			}
			decryptPath(child, segments[1:], resolver, context)
		}
	case []any:
		if segment != "*" {
			return
		}
		for _, child := range value {
			decryptPath(child, segments[1:], resolver, context)
		}
	}
}

func matchingMapKeys(value map[string]any, segment string) []string {
	keys := make([]string, 0, 1)
	for key := range value {
		if strings.EqualFold(key, segment) {
			keys = append(keys, key)
		}
	}
	return keys
}

func decryptValue(value any, resolver Resolver, context string) any {
	switch typed := value.(type) {
	case string:
		return resolver.ResolveOptionalForContext(typed, context)
	case map[string]any:
		for key, child := range typed {
			typed[key] = decryptValue(child, resolver, context)
		}
	case []any:
		for i, child := range typed {
			typed[i] = decryptValue(child, resolver, context)
		}
	}
	return value
}

// Decrypt preserves the public compatibility API for legacy CBC and values
// produced by Encrypt. Registered field-bound v2 values must use
// Resolver.ResolveForContext so their AAD cannot be bypassed.
func Decrypt(encoded string, keyring []string) (string, error) {
	return decryptEncoded(encoded, keyring, "")
}

func decryptEncoded(encoded string, keyring []string, context string) (string, error) {
	encoded = strings.TrimPrefix(encoded, encryptedValuePrefix)
	if strings.HasPrefix(encoded, v2CiphertextPrefix) {
		return decryptV2(encoded, keyring, context)
	}
	return decryptLegacy(encoded, keyring)
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

func decryptLegacy(encoded string, keyring []string) (string, error) {
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
