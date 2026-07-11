package store

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
)

func TestParseConsumerDecryptsEncryptedAuthPluginFields(t *testing.T) {
	key := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	consumer, err := ParseConsumer([]byte(`{
        "username":"alice",
        "plugins":{"key-auth":{"key":"` + encryptForTest(t, key, "api-secret") + `"}}
    }`))
	if err != nil {
		t.Fatalf("ParseConsumer() error = %v", err)
	}
	keyAuth := consumer.Plugins["key-auth"].(map[string]any)
	if got := keyAuth["key"]; got != "api-secret" {
		t.Fatalf("key-auth.key = %v, want decrypted value", got)
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
