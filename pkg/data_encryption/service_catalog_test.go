package data_encryption

import (
	"errors"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func testDeclarationCatalog(t *testing.T) *capability.SecretDeclarationCatalog {
	t.Helper()
	return mustTestDeclarationCatalog()
}

func mustTestDeclarationCatalog() *capability.SecretDeclarationCatalog {
	manifest, err := capability.Load()
	if err != nil {
		panic(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
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

func TestServiceDeclarationDigestAndConfigurationIdentity(t *testing.T) {
	catalog := testDeclarationCatalog(t)
	service := NewService(true, []string{"qeddd145sfvddff3"}, catalog)
	if got, want := service.DeclarationDigest(), catalog.Digest(); got != want {
		t.Fatalf("DeclarationDigest() = %x, want %x", got, want)
	}
	if !service.SameConfiguration(NewService(true, []string{"qeddd145sfvddff3"}, catalog)) {
		t.Fatal("SameConfiguration() rejected identical catalog digest")
	}

	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	manifest.Plugins[0].SecretDeclarations = append(
		manifest.Plugins[0].SecretDeclarations,
		capability.SecretDeclaration{
			Factory: manifest.Plugins[0].Factories[0].Key,
			Source:  capability.SecretPluginConfig,
			Field:   "catalog_mismatch",
		},
	)
	other, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if service.SameConfiguration(NewService(true, []string{"qeddd145sfvddff3"}, other)) {
		t.Fatal("SameConfiguration() accepted a different catalog digest")
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
	manifest := &capability.Manifest{Plugins: []capability.PluginCapability{{
		Name:      "test-owner",
		Factories: []capability.Factory{{Key: "test-plugin"}},
		SecretDeclarations: []capability.SecretDeclaration{
			{Factory: "test-plugin", Source: capability.SecretPluginConfig, Field: "plugin_secret"},
			{Factory: "test-plugin", Source: capability.SecretPluginMetadata, Field: "metadata_secret"},
			{Factory: "test-plugin", Source: capability.SecretConsumerConfig, Field: "consumer_secret"},
		},
	}}}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(true, []string{"qeddd145sfvddff3"}, catalog)

	configs := map[string]any{"test-plugin": map[string]any{
		"plugin_secret":   "plugin-value",
		"consumer_secret": "consumer-value",
	}}
	if err := service.EncryptPluginConfigs(configs); err != nil {
		t.Fatal(err)
	}
	config := configs["test-plugin"].(map[string]any)
	if config["plugin_secret"] == "plugin-value" {
		t.Fatal("EncryptPluginConfigs() did not encrypt plugin_config declaration")
	}
	if got := config["consumer_secret"]; got != "consumer-value" {
		t.Fatalf("EncryptPluginConfigs() consumer_config = %v, want untouched plaintext", got)
	}
	consumerCiphertext, err := EncryptForContext(
		"consumer-value",
		"qeddd145sfvddff3",
		"test-plugin.consumer_secret",
	)
	if err != nil {
		t.Fatal(err)
	}
	config["consumer_secret"] = encryptedValuePrefix + consumerCiphertext
	service.DecryptPluginConfigs(configs)
	if got := config["plugin_secret"]; got != "plugin-value" {
		t.Fatalf("DecryptPluginConfigs() plugin_config = %v, want plugin-value", got)
	}
	if got := config["consumer_secret"]; got != encryptedValuePrefix+consumerCiphertext {
		t.Fatalf("DecryptPluginConfigs() consumer_config = %v, want untouched ciphertext", got)
	}

	metadata := map[string]any{"metadata_secret": "metadata-value", "consumer_secret": "consumer-value"}
	if err := service.EncryptPluginMetadata("test-plugin", metadata); err != nil {
		t.Fatal(err)
	}
	if metadata["metadata_secret"] == "metadata-value" {
		t.Fatal("EncryptPluginMetadata() did not encrypt plugin_metadata declaration")
	}
	if got := metadata["consumer_secret"]; got != "consumer-value" {
		t.Fatalf("EncryptPluginMetadata() consumer_config = %v, want untouched plaintext", got)
	}
	metadata["consumer_secret"] = encryptedValuePrefix + consumerCiphertext
	service.DecryptPluginMetadata("test-plugin", metadata)
	if got := metadata["metadata_secret"]; got != "metadata-value" {
		t.Fatalf("DecryptPluginMetadata() plugin_metadata = %v, want metadata-value", got)
	}
	if got := metadata["consumer_secret"]; got != encryptedValuePrefix+consumerCiphertext {
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

func TestServiceResolveDeclaredPreservesStrictAndOptionalPolicy(t *testing.T) {
	const key = "qeddd145sfvddff3"
	service := NewService(true, []string{key}, testDeclarationCatalog(t))

	ciphertext, err := EncryptForContext("optional-secret", key, "key-auth.key")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.ResolveDeclared("key-auth", capability.SecretPluginConfig, "key", ciphertext); err != nil ||
		got != "optional-secret" {
		t.Fatalf("optional ResolveDeclared() = %q/%v, want optional-secret", got, err)
	}
	if got, err := service.ResolveDeclared(
		"key-auth",
		capability.SecretPluginConfig,
		"key",
		"legacy-plaintext",
	); err != nil ||
		got != "legacy-plaintext" {
		t.Fatalf("optional plaintext ResolveDeclared() = %q/%v, want legacy-plaintext", got, err)
	}

	strictCiphertext, err := EncryptForContext("strict-secret", key, "error-log-logger.clickhouse.password")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := service.ResolveDeclared(
		"error-log-logger",
		capability.SecretPluginConfig,
		"clickhouse.password",
		strictCiphertext,
	); err != nil ||
		got != "strict-secret" {
		t.Fatalf("strict ResolveDeclared() = %q/%v, want strict-secret", got, err)
	}
	if _, err := service.ResolveDeclared(
		"error-log-logger",
		capability.SecretPluginConfig,
		"clickhouse.password",
		"legacy-plaintext",
	); !errors.Is(
		err,
		ErrInvalidCiphertext,
	) {
		t.Fatalf("strict plaintext ResolveDeclared() error = %v, want ErrInvalidCiphertext", err)
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
