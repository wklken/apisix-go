package data_encryption

import (
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func testDeclarationCatalog(t *testing.T) *capability.SecretDeclarationCatalog {
	t.Helper()
	return mustTestDeclarationCatalog()
}

func mustTestDeclarationCatalog() *capability.SecretDeclarationCatalog {
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		panic(err)
	}
	return catalog
}

func testService(t *testing.T, enabled bool, keyring []string) Service {
	t.Helper()
	return NewService(enabled, keyring, testDeclarationCatalog(t))
}

func TestNewServiceRejectsNilDeclarationCatalog(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != ErrDeclarationCatalogUnavailable {
			t.Fatalf("NewService() panic = %v, want %v", recovered, ErrDeclarationCatalogUnavailable)
		}
	}()
	_ = NewService(false, nil, nil)
}

func TestServiceDeclarationDigest(t *testing.T) {
	catalog := testDeclarationCatalog(t)
	service := NewService(true, []string{"qeddd145sfvddff3"}, catalog)
	if got, want := service.DeclarationDigest(), catalog.Digest(); got != want {
		t.Fatalf("DeclarationDigest() = %x, want %x", got, want)
	}
}

func TestServiceValidatesDeclarationsAndSeparatesSources(t *testing.T) {
	service := NewService(true, []string{"qeddd145sfvddff3"}, testDeclarationCatalog(t))
	declaration, err := service.ValidateDeclaration("key-auth", capability.SecretPluginConfig, "key")
	if err != nil {
		t.Fatal(err)
	}
	if declaration.Factory != "key-auth" || declaration.Source != capability.SecretPluginConfig ||
		declaration.Field != "key" {
		t.Fatalf("declaration = %+v", declaration)
	}
	if _, err := service.ValidateDeclaration("key-auth", capability.SecretPluginMetadata, "key"); err == nil {
		t.Fatal("ValidateDeclaration() accepted a declaration from the wrong source")
	}
	if _, err := service.ValidateDeclaration("key-auth", capability.SecretPluginConfig, "unknown"); err == nil {
		t.Fatal("ValidateDeclaration() accepted an undeclared field")
	}

	if !service.HasEncryptedPluginMetadata("azure-functions") {
		t.Fatal("HasEncryptedPluginMetadata() = false, want true")
	}
	if service.HasEncryptedPluginMetadata("key-auth") {
		t.Fatal("HasEncryptedPluginMetadata() = true for config-only plugin")
	}
}

func TestPluginAndMetadataEncryptionIgnoreConsumerDeclarations(t *testing.T) {
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(true, []string{"qeddd145sfvddff3"}, catalog)

	configs := map[string]any{"jwt-auth": map[string]any{
		"secret": "plugin-value",
		"key":    "consumer-value",
	}}
	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	config := configs["jwt-auth"].(map[string]any)
	if config["secret"] == "plugin-value" {
		t.Fatal("EncryptPluginConfigs() did not encrypt plugin_config declaration")
	}
	if got := config["key"]; got != "consumer-value" {
		t.Fatalf("EncryptPluginConfigs() consumer_config = %v, want untouched plaintext", got)
	}
	consumerCiphertext, err := EncryptForContext(
		"consumer-value",
		"qeddd145sfvddff3",
		"jwt-auth.key",
	)
	if err != nil {
		t.Fatal(err)
	}
	config["key"] = encryptedValuePrefix + consumerCiphertext
	service.DecryptPluginConfigs(configs)
	if got := config["secret"]; got != "plugin-value" {
		t.Fatalf("DecryptPluginConfigs() plugin_config = %v, want plugin-value", got)
	}
	if got := config["key"]; got != encryptedValuePrefix+consumerCiphertext {
		t.Fatalf("DecryptPluginConfigs() consumer_config = %v, want untouched ciphertext", got)
	}

	metadata := map[string]any{"master_apikey": "metadata-value", "key": "consumer-value"}
	if err := service.EncryptPluginMetadata("azure-functions", metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["master_apikey"] == "metadata-value" {
		t.Fatal("EncryptPluginMetadata() did not encrypt plugin_metadata declaration")
	}
	if got := metadata["key"]; got != "consumer-value" {
		t.Fatalf("EncryptPluginMetadata() consumer_config = %v, want untouched plaintext", got)
	}
	metadata["key"] = encryptedValuePrefix + consumerCiphertext
	service.DecryptPluginMetadata("azure-functions", metadata)
	if got := metadata["master_apikey"]; got != "metadata-value" {
		t.Fatalf("DecryptPluginMetadata() plugin_metadata = %v, want metadata-value", got)
	}
	if got := metadata["key"]; got != encryptedValuePrefix+consumerCiphertext {
		t.Fatalf("DecryptPluginMetadata() consumer_config = %v, want untouched ciphertext", got)
	}
}

func TestServiceRejectsUnknownDeclarationBeforeResolving(t *testing.T) {
	service := NewService(true, []string{"qeddd145sfvddff3"}, testDeclarationCatalog(t))
	_, err := service.ResolveDeclared("key-auth", capability.SecretPluginMetadata, "key", "secret-value")
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("ResolveDeclared() error = %v, want redacted undeclared declaration error", err)
	}
}

func TestServiceResolveDeclaredUsesAPISIXFallbackPolicy(t *testing.T) {
	const key = "qeddd145sfvddff3"
	service := NewService(true, []string{key}, testDeclarationCatalog(t))

	ciphertext, err := EncryptForContext("secret", key, "error-log-logger.clickhouse.password")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "decrypts with configured key", value: ciphertext, want: "secret"},
		{name: "preserves plaintext", value: "plaintext", want: "plaintext"},
		{
			name:  "preserves ciphertext when no key can decrypt it",
			value: "$encrypted://v2:not-valid-ciphertext",
			want:  "$encrypted://v2:not-valid-ciphertext",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, resolveErr := service.ResolveDeclared(
				"error-log-logger",
				capability.SecretPluginConfig,
				"clickhouse.password",
				test.value,
			)
			if resolveErr != nil || got != test.want {
				t.Fatalf("ResolveDeclared() = %q/%v, want %q/nil", got, resolveErr, test.want)
			}
		})
	}
}

func TestZeroServiceResolverFailsClosed(t *testing.T) {
	assertPanicsWithCatalogError(t, func() { _ = (Service{}).Enabled() })
	assertPanicsWithCatalogError(t, func() { _ = (Service{}).Resolver() })
	var resolver Resolver
	assertPanicsWithCatalogError(t, func() { _, _ = resolver.Resolve("secret") })
	assertPanicsWithCatalogError(t, func() { _ = resolver.ResolveOptional("secret") })
	assertPanicsWithCatalogError(t, func() { (Service{}).DecryptPluginConfigs(nil) })
	assertPanicsWithCatalogError(t, func() {
		(Service{}).DecryptPluginConfigWithResolver(nil, "unknown", Resolver{})
	})
	assertPanicsWithCatalogError(t, func() { (Service{}).DecryptPluginMetadata("azure-functions", nil) })
	assertPanicsWithCatalogError(t, func() {
		DecryptPluginConfigWithResolver(nil, "unknown", Resolver{}, mustTestDeclarationCatalog())
	})
	if _, err := (Service{}).ValidateDeclaration(
		"key-auth",
		capability.SecretPluginConfig,
		"key",
	); err != ErrDeclarationCatalogUnavailable {
		t.Fatalf("zero service ValidateDeclaration() error = %v, want %v", err, ErrDeclarationCatalogUnavailable)
	}
	assertPanicsWithCatalogError(t, func() { _ = (Service{}).HasEncryptedPluginMetadata("azure-functions") })
}

func assertPanicsWithCatalogError(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != ErrDeclarationCatalogUnavailable {
			t.Fatalf("panic = %v, want %v", recovered, ErrDeclarationCatalogUnavailable)
		}
	}()
	call()
}
