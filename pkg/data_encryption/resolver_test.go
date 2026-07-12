package data_encryption

import (
	"errors"
	"testing"
)

func TestResolverDecryptsWithRotatedKeyring(t *testing.T) {
	oldKey := "old-keyring-item"
	resolver := NewResolver(true, []string{"new-keyring-item", oldKey})
	ciphertext := encryptForTest(t, oldKey, "access-token")

	plaintext, err := resolver.Resolve(ciphertext)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if plaintext != "access-token" {
		t.Fatalf("plaintext = %q, want access-token", plaintext)
	}
}

func TestResolverRejectsInvalidCiphertext(t *testing.T) {
	resolver := NewResolver(true, []string{"qeddd145sfvddff3"})

	_, err := resolver.Resolve("not-a-ciphertext")
	if !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("Resolve() error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestResolverRejectsMissingKey(t *testing.T) {
	resolver := NewResolver(true, nil)

	_, err := resolver.Resolve("ciphertext")
	if !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("Resolve() error = %v, want ErrKeyUnavailable", err)
	}
}

func TestResolverLeavesPlaintextWhenEncryptionIsDisabled(t *testing.T) {
	resolver := NewResolver(false, nil)

	plaintext, err := resolver.Resolve("plain-value")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if plaintext != "plain-value" {
		t.Fatalf("plaintext = %q, want plain-value", plaintext)
	}
}

func TestRedactDoesNotReturnSecret(t *testing.T) {
	if got := Redact("access-token"); got == "access-token" || got != redactedValue {
		t.Fatalf("Redact() = %q, want fixed redaction", got)
	}
	if got := Redact(""); got != "" {
		t.Fatalf("Redact(empty) = %q, want empty", got)
	}
}
