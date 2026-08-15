package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// ResolvedSecret owns one resolved credential for a plugin generation. The
// plaintext is never exposed through its descriptor and every read is cloned.
type ResolvedSecret struct {
	mu          sync.RWMutex
	value       []byte
	reference   string
	version     string
	fingerprint string
}

// MaterializeSecret resolves a literal, environment, or managed reference
// into a generation-owned handle.
func MaterializeSecret(value string) (*ResolvedSecret, error) {
	resolved, err := ResolveSecretReference(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(resolved))
	fingerprint := hex.EncodeToString(digest[:])
	reference := "$literal"
	if isSecretReference(value) {
		reference = value
	}
	return &ResolvedSecret{
		value:       []byte(resolved),
		reference:   reference,
		version:     fingerprint[:16],
		fingerprint: fingerprint,
	}, nil
}

func isSecretReference(value string) bool {
	upper := strings.ToUpper(value)
	return strings.HasPrefix(upper, environmentSecretPrefix) || strings.HasPrefix(value, managedSecretPrefix)
}

// Bytes returns a caller-owned copy, or nil after destruction.
func (s *ResolvedSecret) Bytes() []byte {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.value...)
}

// Reference returns the original reference, or "$literal" for literal input.
func (s *ResolvedSecret) Reference() string {
	if s == nil {
		return ""
	}
	return s.reference
}

// Version is a stable short content version for generation comparison.
func (s *ResolvedSecret) Version() string {
	if s == nil {
		return ""
	}
	return s.version
}

// Fingerprint returns the full SHA-256 fingerprint without exposing plaintext.
func (s *ResolvedSecret) Fingerprint() string {
	if s == nil {
		return ""
	}
	return s.fingerprint
}

// Descriptor is safe for effective configuration and diagnostics.
func (s *ResolvedSecret) Descriptor() string {
	if s == nil {
		return ""
	}
	return s.reference + "#sha256:" + s.fingerprint
}

// Destroy best-effort zeroes the owned bytes and is idempotent.
func (s *ResolvedSecret) Destroy() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.value {
		s.value[index] = 0
	}
	s.value = nil
}
