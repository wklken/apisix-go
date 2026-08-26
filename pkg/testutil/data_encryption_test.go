package testutil

import "testing"

func TestDataEncryptionServiceUsesManifestCatalogAndKeyring(t *testing.T) {
	const (
		key     = "0123456789abcdef"
		context = "testutil.secret"
	)
	service := DataEncryptionService(true, []string{key})
	if !service.Configured() || !service.Enabled() {
		t.Fatal("DataEncryptionService() did not return an enabled manifest-backed service")
	}
	ciphertext, err := service.EncryptForContext("secret", context)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	plaintext, err := DataEncryptionResolver(true, []string{key}).ResolveForContext(ciphertext, context)
	if err != nil {
		t.Fatalf("DataEncryptionResolver().ResolveForContext() error = %v", err)
	}
	if plaintext != "secret" {
		t.Fatalf("resolved plaintext = %q, want secret", plaintext)
	}
}

func TestUnconfiguredDataEncryptionServiceRemainsUnconfigured(t *testing.T) {
	if UnconfiguredDataEncryptionService().Configured() {
		t.Fatal("UnconfiguredDataEncryptionService() reported configured")
	}
}
