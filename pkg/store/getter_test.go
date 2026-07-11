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

func TestDecodePluginMetadataDecryptsAzureMasterAPIKey(t *testing.T) {
	key := "qeddd145sfvddff3"
	data_encryption.Configure(true, []string{key})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	var metadata struct {
		MasterAPIKey   string `json:"master_apikey"`
		MasterClientID string `json:"master_clientid"`
	}
	err := decodePluginMetadata([]byte(`{
        "master_apikey":"`+encryptForTest(t, key, "master-key")+`",
        "master_clientid":"master-client"
    }`), "azure-functions", &metadata)
	if err != nil {
		t.Fatalf("decodePluginMetadata() error = %v", err)
	}
	if metadata.MasterAPIKey != "master-key" || metadata.MasterClientID != "master-client" {
		t.Fatalf("metadata = %#v, want decrypted master key", metadata)
	}
}

func TestDecodePluginMetadataPreservesUnregisteredLargeIntegers(t *testing.T) {
	data_encryption.Configure(true, []string{"qeddd145sfvddff3"})
	t.Cleanup(func() { data_encryption.Configure(false, nil) })

	var metadata struct {
		Sequence int64 `json:"sequence"`
	}
	if err := decodePluginMetadata([]byte(`{"sequence":9007199254740993}`), "example-plugin", &metadata); err != nil {
		t.Fatalf("decodePluginMetadata() error = %v", err)
	}
	if metadata.Sequence != 9007199254740993 {
		t.Fatalf("sequence = %d, want exact large integer", metadata.Sequence)
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
