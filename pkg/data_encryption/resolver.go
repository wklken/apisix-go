package data_encryption

import (
	"errors"
	"fmt"
)

const redactedValue = "[REDACTED]"

var (
	ErrKeyUnavailable    = errors.New("secret key unavailable")
	ErrInvalidCiphertext = errors.New("invalid secret ciphertext")
)

// Resolver decrypts APISIX data-encryption values without exposing key material.
// The keyring is ordered from newest to oldest so rotated values remain readable.
type Resolver struct {
	enabled bool
	keyring []string
}

func NewResolver(enabled bool, keyring []string) Resolver {
	return Resolver{
		enabled: enabled,
		keyring: append([]string(nil), keyring...),
	}
}

// Resolve strictly resolves a value that is expected to be encrypted when
// encryption is enabled. Callers that need legacy plaintext compatibility can
// use ResolveOptional instead.
func (r Resolver) Resolve(value string) (string, error) {
	if value == "" || !r.enabled {
		return value, nil
	}
	if len(r.keyring) == 0 {
		return "", ErrKeyUnavailable
	}

	plaintext, err := Decrypt(value, r.keyring)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidCiphertext, err)
	}
	return plaintext, nil
}

// ResolveOptional decrypts a value when possible and preserves legacy
// plaintext values when encryption is disabled or the value is not encrypted.
func (r Resolver) ResolveOptional(value string) string {
	plaintext, err := r.Resolve(value)
	if err != nil {
		return value
	}
	return plaintext
}

func Redact(value string) string {
	if value == "" {
		return ""
	}
	return redactedValue
}
