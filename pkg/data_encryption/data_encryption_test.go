package data_encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
)

func TestDecryptPluginConfigsUsesKeyringAndNestedFields(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"openid-connect": map[string]any{
			"client_secret": encryptForTest(t, key, "client-secret"),
			"session": map[string]any{
				"redis": map[string]any{"password": encryptForTest(t, key, "redis-password")},
			},
		},
	}

	DecryptPluginConfigs(configs, []string{"old-keyring-item", key})
	oidc := configs["openid-connect"].(map[string]any)
	if got := oidc["client_secret"]; got != "client-secret" {
		t.Fatalf("client_secret = %v, want plaintext", got)
	}
	if got := oidc["session"].(map[string]any)["redis"].(map[string]any)["password"]; got != "redis-password" {
		t.Fatalf("session.redis.password = %v, want plaintext", got)
	}
}

func TestDecryptPluginConfigsSupportsAIMapsAndInstanceArrays(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"ai-proxy": map[string]any{"auth": map[string]any{
			"header": map[string]any{"Authorization": encryptForTest(t, key, "Bearer secret")},
			"aws":    map[string]any{"secret_access_key": encryptForTest(t, key, "aws-secret")},
		}},
		"ai-proxy-multi": map[string]any{"instances": []any{
			map[string]any{"auth": map[string]any{
				"query": map[string]any{"api-key": encryptForTest(t, key, "query-secret")},
			}},
		}},
		"ai-rag": map[string]any{
			"embeddings_provider": map[string]any{"azure_openai": map[string]any{
				"api_key": encryptForTest(t, key, "embedding-secret"),
			}},
		},
	}

	DecryptPluginConfigs(configs, []string{key})
	proxyAuth := configs["ai-proxy"].(map[string]any)["auth"].(map[string]any)
	if proxyAuth["header"].(map[string]any)["Authorization"] != "Bearer secret" ||
		proxyAuth["aws"].(map[string]any)["secret_access_key"] != "aws-secret" {
		t.Fatalf("ai-proxy auth = %#v", proxyAuth)
	}
	instance := configs["ai-proxy-multi"].(map[string]any)["instances"].([]any)[0].(map[string]any)
	if instance["auth"].(map[string]any)["query"].(map[string]any)["api-key"] != "query-secret" {
		t.Fatalf("ai-proxy-multi instance = %#v", instance)
	}
	rawRAG := configs["ai-rag"].(map[string]any)
	if rawRAG["embeddings_provider"].(map[string]any)["azure_openai"].(map[string]any)["api_key"] !=
		"embedding-secret" {
		t.Fatalf("ai-rag config = %#v", rawRAG)
	}
}

func TestDecryptPluginConfigsSupportsServerlessCredentials(t *testing.T) {
	key := "qeddd145sfvddff3"
	configs := map[string]any{
		"aws-lambda": map[string]any{"authorization": map[string]any{
			"apikey": encryptForTest(t, key, "aws-api-key"),
			"iam": map[string]any{
				"accesskey": encryptForTest(t, key, "aws-access-key"),
				"secretkey": encryptForTest(t, key, "aws-secret-key"),
			},
		}},
		"azure-functions": map[string]any{"authorization": map[string]any{
			"apikey": encryptForTest(t, key, "azure-api-key"),
		}},
		"openfunction": map[string]any{"authorization": map[string]any{
			"service_token": encryptForTest(t, key, "openfunction-token"),
		}},
		"openwhisk": map[string]any{
			"service_token": encryptForTest(t, key, "openwhisk-token"),
		},
	}

	DecryptPluginConfigs(configs, []string{key})
	aws := configs["aws-lambda"].(map[string]any)["authorization"].(map[string]any)
	if aws["apikey"] != "aws-api-key" ||
		aws["iam"].(map[string]any)["accesskey"] != "aws-access-key" ||
		aws["iam"].(map[string]any)["secretkey"] != "aws-secret-key" {
		t.Fatalf("aws-lambda authorization = %#v", aws)
	}
	azure := configs["azure-functions"].(map[string]any)["authorization"].(map[string]any)
	if azure["apikey"] != "azure-api-key" {
		t.Fatalf("azure-functions authorization = %#v", azure)
	}
	openFunction := configs["openfunction"].(map[string]any)["authorization"].(map[string]any)
	if openFunction["service_token"] != "openfunction-token" {
		t.Fatalf("openfunction authorization = %#v", openFunction)
	}
	openWhisk := configs["openwhisk"].(map[string]any)
	if openWhisk["service_token"] != "openwhisk-token" {
		t.Fatalf("openwhisk config = %#v", openWhisk)
	}
}

func encryptForTest(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
