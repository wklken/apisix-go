package data_encryption

import (
	"errors"
	"strings"
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

func TestEncryptForContextProducesNondeterministicV2Ciphertext(t *testing.T) {
	key := "qeddd145sfvddff3"
	context := "kafka-proxy.sasl.password"
	first, err := EncryptForContext("access-token", key, context)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	second, err := EncryptForContext("access-token", key, context)
	if err != nil {
		t.Fatalf("EncryptForContext() second error = %v", err)
	}
	if !strings.HasPrefix(first, "v2:") || !strings.HasPrefix(second, "v2:") {
		t.Fatalf("ciphertexts = %q/%q, want v2 envelopes", first, second)
	}
	if first == second {
		t.Fatal("EncryptForContext() returned deterministic ciphertext")
	}

	resolver := NewResolver(true, []string{key})
	plaintext, err := resolver.ResolveForContext(first, context)
	if err != nil || plaintext != "access-token" {
		t.Fatalf("ResolveForContext() = (%q, %v), want access-token", plaintext, err)
	}
}

func TestResolverDecryptsExplicitV2Wrapper(t *testing.T) {
	key := "qeddd145sfvddff3"
	context := "kafka-proxy.sasl.password"
	ciphertext, err := EncryptForContext("access-token", key, context)
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}

	plaintext, err := NewResolver(true, []string{key}).ResolveForContext(
		encryptedValuePrefix+ciphertext,
		context,
	)
	if err != nil || plaintext != "access-token" {
		t.Fatalf("ResolveForContext() = (%q, %v), want access-token", plaintext, err)
	}
}

func TestResolverRejectsV2TamperingAndWrongContext(t *testing.T) {
	key := "qeddd145sfvddff3"
	ciphertext, err := EncryptForContext("access-token", key, "kafka-proxy.sasl.password")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	encoded := strings.TrimPrefix(ciphertext, "v2:")
	last := encoded[len(encoded)-1]
	if last == 'A' {
		last = 'B'
	} else {
		last = 'A'
	}
	tampered := "v2:" + encoded[:len(encoded)-1] + string(last)
	resolver := NewResolver(true, []string{key})
	if _, err := resolver.ResolveForContext(
		tampered,
		"kafka-proxy.sasl.password",
	); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("tampered ResolveForContext() error = %v, want ErrInvalidCiphertext", err)
	}
	if _, err := resolver.ResolveForContext(
		ciphertext,
		"kafka-logger.brokers.*.sasl_config.password",
	); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("wrong-context ResolveForContext() error = %v, want ErrInvalidCiphertext", err)
	}
}

func TestDecryptDoesNotBypassContextBinding(t *testing.T) {
	key := "qeddd145sfvddff3"
	ciphertext, err := EncryptForContext("access-token", key, "kafka-proxy.sasl.password")
	if err != nil {
		t.Fatalf("EncryptForContext() error = %v", err)
	}
	if _, err := Decrypt(ciphertext, []string{key}); err == nil {
		t.Fatal("Decrypt() accepted a field-bound v2 ciphertext without context")
	}
}
