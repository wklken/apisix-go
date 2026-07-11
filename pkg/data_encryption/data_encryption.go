package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
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
	"authz-keycloak":  {"client_secret"},
	"authz-casdoor":   {"client_secret"},
	"aws-lambda":      {"authorization.apikey", "authorization.iam.accesskey", "authorization.iam.secretkey"},
	"azure-functions": {"authorization.apikey"},
	"basic-auth":      {"password"},
	"cas-auth":        {"cookie.secret"},
	"dingtalk-auth":   {"app_secret", "secret"},
	"feishu-auth":     {"app_secret", "secret"},
	"hmac-auth":       {"secret"},
	"jwe-decrypt":     {"key", "secret"},
	"jwt-auth":        {"secret", "private_key"},
	"key-auth":        {"key"},
	"ldap-auth":       {"user_dn"},
	"openid-connect":  {"client_secret", "client_rsa_private_key", "session.secret", "session.redis.password"},
	"openfunction":    {"authorization.service_token"},
	"openwhisk":       {"service_token"},
	"saml-auth":       {"sp_private_key", "secret"},
}

var pluginMetadataFields = map[string][]string{
	"azure-functions": {"master_apikey"},
}

func HasEncryptedPluginMetadata(name string) bool {
	return len(pluginMetadataFields[name]) != 0
}

func DecryptPluginConfigs(configs map[string]any, keyring []string) {
	if len(keyring) == 0 {
		return
	}
	for name, fields := range pluginFields {
		config, ok := configs[name].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range fields {
			decryptField(config, field, keyring)
		}
	}
}

func DecryptPluginMetadata(name string, metadata map[string]any, keyring []string) {
	if len(keyring) == 0 {
		return
	}
	for _, field := range pluginMetadataFields[name] {
		decryptField(metadata, field, keyring)
	}
}

func decryptField(config map[string]any, path string, keyring []string) {
	decryptPath(config, strings.Split(path, "."), keyring)
}

func decryptPath(current any, segments []string, keyring []string) {
	if len(segments) == 0 {
		return
	}
	segment := segments[0]
	switch value := current.(type) {
	case map[string]any:
		if segment == "*" {
			for _, child := range value {
				decryptPath(child, segments[1:], keyring)
			}
			return
		}
		child, ok := value[segment]
		if !ok {
			return
		}
		if len(segments) == 1 {
			value[segment] = decryptValue(child, keyring)
			return
		}
		decryptPath(child, segments[1:], keyring)
	case []any:
		if segment != "*" {
			return
		}
		for _, child := range value {
			decryptPath(child, segments[1:], keyring)
		}
	}
}

func decryptValue(value any, keyring []string) any {
	switch typed := value.(type) {
	case string:
		if plain, err := Decrypt(typed, keyring); err == nil {
			return plain
		}
	case map[string]any:
		for key, child := range typed {
			typed[key] = decryptValue(child, keyring)
		}
	case []any:
		for i, child := range typed {
			typed[i] = decryptValue(child, keyring)
		}
	}
	return value
}

func Decrypt(encoded string, keyring []string) (string, error) {
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
