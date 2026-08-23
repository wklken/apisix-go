package store

import (
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/data_encryption"
)

func testDataEncryption() data_encryption.Service {
	return data_encryption.NewService(false, nil)
}

func testParsingStore() *Store {
	return &Store{dataEncryption: testDataEncryption()}
}

func TestStoresUseOnlyTheirOwnDataEncryptionService(t *testing.T) {
	firstKey := "qeddd145sfvddff3"
	secondKey := "1234567890abcdef"
	first, err := Open(
		t.TempDir()+"/first.db",
		make(chan *Event),
		data_encryption.NewService(true, []string{firstKey}),
	)
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Stop() })
	second, err := Open(
		t.TempDir()+"/second.db",
		make(chan *Event),
		data_encryption.NewService(true, []string{secondKey}),
	)
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Stop() })

	firstCiphertext := encryptForTest(t, firstKey, "first-secret")
	secondCiphertext := encryptForTest(t, secondKey, "second-secret")
	firstConsumer, err := first.ParseConsumer([]byte(`{
		"username":"first",
		"plugins":{"key-auth":{"key":"` + firstCiphertext + `"}}
	}`))
	if err != nil {
		t.Fatalf("first.ParseConsumer() error = %v", err)
	}
	secondConsumer, err := second.ParseConsumer([]byte(`{
		"username":"second",
		"plugins":{"key-auth":{"key":"` + secondCiphertext + `"}}
	}`))
	if err != nil {
		t.Fatalf("second.ParseConsumer() error = %v", err)
	}
	if got := firstConsumer.Plugins["key-auth"].(map[string]any)["key"]; got != "first-secret" {
		t.Fatalf("first key = %v, want first-secret", got)
	}
	if got := secondConsumer.Plugins["key-auth"].(map[string]any)["key"]; got != "second-secret" {
		t.Fatalf("second key = %v, want second-secret", got)
	}

	cross, err := first.ParseConsumer([]byte(`{
		"username":"cross",
		"plugins":{"key-auth":{"key":"` + secondCiphertext + `"}}
	}`))
	if err != nil {
		t.Fatalf("first.ParseConsumer(cross) error = %v", err)
	}
	if got := cross.Plugins["key-auth"].(map[string]any)["key"]; got == "second-secret" {
		t.Fatal("first Store decrypted ciphertext owned by the second Store")
	}
}

func TestGetStoreRequiresMatchingDataEncryptionService(t *testing.T) {
	path := t.TempDir() + "/global.db"
	service := data_encryption.NewService(true, []string{"qeddd145sfvddff3"})
	first, err := GetStore(path, make(chan *Event), service)
	if err != nil {
		t.Fatalf("first GetStore() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Stop() })

	second, err := GetStore(path, make(chan *Event), data_encryption.NewService(true, []string{"qeddd145sfvddff3"}))
	if err != nil {
		t.Fatalf("matching GetStore() error = %v", err)
	}
	if second != first {
		t.Fatal("matching GetStore() did not reuse the singleton")
	}

	_, err = GetStore(
		path,
		make(chan *Event),
		data_encryption.NewService(true, []string{"1234567890abcdef"}),
	)
	if err == nil || !strings.Contains(
		err.Error(),
		"global store already initialized with a different data-encryption service",
	) {
		t.Fatalf("mismatched GetStore() error = %v", err)
	}
}
