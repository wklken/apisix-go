package data_encryption

import (
	"errors"
	"strings"
	"testing"
)

func TestServiceInstancesDoNotShareKeyrings(t *testing.T) {
	first := NewService(true, []string{"qeddd145sfvddff3"})
	second := NewService(false, nil)

	ciphertext, err := first.EncryptForContext("secret", "test.secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Resolver().ResolveForContext(ciphertext, "test.secret"); err != nil {
		t.Fatal(err)
	}
	if got, err := second.Resolver().ResolveForContext("plain", "test.secret"); err != nil || got != "plain" {
		t.Fatalf("disabled resolver = (%q, %v)", got, err)
	}
	if _, err := second.Resolver().ResolveForContext(ciphertext, "test.secret"); err != nil {
		t.Fatalf("disabled resolver read another service's keyring: %v", err)
	}
}

func TestEnabledServiceCannotDecryptAnotherServiceCiphertext(t *testing.T) {
	first := NewService(true, []string{"qeddd145sfvddff3"})
	second := NewService(true, []string{"old-keyring-item"})
	ciphertext, err := first.EncryptForContext("secret", "test.secret")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := second.Resolver().ResolveForContext(ciphertext, "test.secret"); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("second service ResolveForContext() error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestServiceClonesConstructorKeyring(t *testing.T) {
	keyring := []string{"qeddd145sfvddff3"}
	service := NewService(true, keyring)
	keyring[0] = "changed-key-item"

	ciphertext, err := service.EncryptForContext("secret", "test.secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Resolver().ResolveForContext(ciphertext, "test.secret")
	if err != nil || got != "secret" {
		t.Fatalf("ResolveForContext() = (%q, %v), want secret", got, err)
	}
}

func TestServiceResolverDoesNotShareMutableKeyring(t *testing.T) {
	keyring := []string{"qeddd145sfvddff3"}
	service := NewService(true, keyring)
	resolver := service.Resolver()
	keyring[0] = "changed-key-item"

	ciphertext, err := service.EncryptForContext("secret", "test.secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.ResolveForContext(ciphertext, "test.secret")
	if err != nil || got != "secret" {
		t.Fatalf("ResolveForContext() = (%q, %v), want secret", got, err)
	}
}

func TestServiceSameConfiguration(t *testing.T) {
	keyring := []string{"qeddd145sfvddff3", "old-keyring-item"}
	tests := []struct {
		name  string
		left  Service
		right Service
		want  bool
	}{
		{
			name:  "same",
			left:  NewService(true, keyring),
			right: NewService(true, append([]string(nil), keyring...)),
			want:  true,
		},
		{
			name:  "different order",
			left:  NewService(true, keyring),
			right: NewService(true, []string{keyring[1], keyring[0]}),
		},
		{
			name:  "different enabled",
			left:  NewService(true, keyring),
			right: NewService(false, keyring),
		},
		{
			name:  "different keyring",
			left:  NewService(true, keyring),
			right: NewService(true, []string{keyring[0]}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.left.SameConfiguration(tt.right); got != tt.want {
				t.Fatalf("SameConfiguration() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestServiceEncryptsAndDecryptsPluginConfigs(t *testing.T) {
	service := NewService(true, []string{"qeddd145sfvddff3"})
	configs := map[string]any{
		"key-auth": map[string]any{"key": "api-secret"},
	}

	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	ciphertext, ok := configs["key-auth"].(map[string]any)["key"].(string)
	if !ok || ciphertext == "api-secret" {
		t.Fatalf("encrypted key = %#v", configs["key-auth"].(map[string]any)["key"])
	}
	service.DecryptPluginConfigs(configs)
	if got := configs["key-auth"].(map[string]any)["key"]; got != "api-secret" {
		t.Fatalf("decrypted key = %#v, want api-secret", got)
	}
}

func TestServiceEncryptsAndDecryptsPluginMetadata(t *testing.T) {
	service := NewService(true, []string{"qeddd145sfvddff3"})
	metadata := map[string]any{"master_apikey": "api-secret"}

	if err := service.EncryptPluginMetadata("azure-functions", metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["master_apikey"] == "api-secret" {
		t.Fatal("metadata was not encrypted")
	}
	service.DecryptPluginMetadata("azure-functions", metadata)
	if got := metadata["master_apikey"]; got != "api-secret" {
		t.Fatalf("decrypted metadata = %#v, want api-secret", got)
	}
}

func TestDisabledServiceEncryptionIsNoOp(t *testing.T) {
	service := NewService(false, nil)
	configs := map[string]any{"key-auth": map[string]any{"key": "api-secret"}}
	metadata := map[string]any{"master_apikey": "api-secret"}

	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	if err := service.EncryptPluginMetadata("azure-functions", metadata); err != nil {
		t.Fatal(err)
	}
	if got := configs["key-auth"].(map[string]any)["key"]; got != "api-secret" {
		t.Fatalf("plugin config = %#v, want unchanged", got)
	}
	if got := metadata["master_apikey"]; got != "api-secret" {
		t.Fatalf("plugin metadata = %#v, want unchanged", got)
	}
}

func TestServiceErrorsDoNotExposeSecretValues(t *testing.T) {
	service := NewService(true, []string{"qeddd145sfvddff3"})
	secret := "must-not-appear"
	configs := map[string]any{
		"key-auth": map[string]any{"key": encryptedValuePrefix + secret},
	}
	metadata := map[string]any{"master_apikey": encryptedValuePrefix + secret}

	for _, err := range []error{
		service.EncryptPluginConfigs(configs),
		service.EncryptPluginMetadata("azure-functions", metadata),
	} {
		if err == nil {
			t.Fatal("invalid explicit ciphertext was accepted")
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error exposed secret value: %v", err)
		}
	}
}
