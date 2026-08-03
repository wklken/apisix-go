package store

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
	bolt "go.etcd.io/bbolt"
)

func TestResolveSecretReferencePreservesLiteral(t *testing.T) {
	original := s
	s = nil
	t.Cleanup(func() { s = original })

	got, err := ResolveSecretReference("literal-access-key")
	if err != nil {
		t.Fatalf("ResolveSecretReference() error = %v", err)
	}
	if got != "literal-access-key" {
		t.Fatalf("ResolveSecretReference() = %q, want literal-access-key", got)
	}
}

func TestResolveSecretReferenceResolvesEnvironmentValue(t *testing.T) {
	const name = "APISIX_GO_STORE_SECRET_REFERENCE_TEST"
	t.Setenv(name, "environment-secret")

	got, err := ResolveSecretReference("$ENV://" + name)
	if err != nil {
		t.Fatalf("ResolveSecretReference() error = %v", err)
	}
	if got != "environment-secret" {
		t.Fatalf("ResolveSecretReference() = %q, want environment-secret", got)
	}
}

func TestResolveSecretReferenceRequiresInitializedStoreForVault(t *testing.T) {
	original := s
	s = nil
	t.Cleanup(func() { s = original })

	_, err := ResolveSecretReference("$secret://vault/test1/foo/access_key_id")
	if err == nil || err.Error() != "secret store is not initialized" {
		t.Fatalf("ResolveSecretReference() error = %v, want uninitialized Store error", err)
	}
}

func TestResolveSecretReferenceResolvesVaultValue(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/kv/apisix/foo" {
			t.Fatalf("Vault request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("X-Vault-Token"); got != "root" {
			t.Fatalf("X-Vault-Token = %q, want root", got)
		}
		_, _ = w.Write([]byte(`{"data":{"access_key_id":"vault-access-key"}}`))
	}))
	defer vault.Close()

	secretStore := newConsumerSnapshotStore(t)
	config, err := json.Marshal(vaultSecretConfig{
		URI: vault.URL, Prefix: "kv/apisix", Token: "root",
	})
	if err != nil {
		t.Fatalf("encode Vault config: %v", err)
	}
	if err := secretStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put([]byte("vault/test1"), config)
	}); err != nil {
		t.Fatalf("store Vault config: %v", err)
	}

	original := s
	s = secretStore
	t.Cleanup(func() { s = original })

	got, err := ResolveSecretReference("$secret://vault/test1/foo/access_key_id")
	if err != nil {
		t.Fatalf("ResolveSecretReference() error = %v", err)
	}
	if got != "vault-access-key" {
		t.Fatalf("ResolveSecretReference() = %q, want vault-access-key", got)
	}
}

func TestResolveSecretReferenceErrorsDoNotExposeEnvironmentValue(t *testing.T) {
	const name = "APISIX_GO_STORE_SECRET_REFERENCE_JSON_TEST"
	const secret = "must-not-appear"
	if err := os.Setenv(name, `{"field":42,"credential":"`+secret+`"}`); err != nil {
		t.Fatalf("Setenv() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Unsetenv(name) })

	_, err := ResolveSecretReference("$ENV://" + name + "/field")
	if err == nil {
		t.Fatal("ResolveSecretReference() error = nil, want non-string field error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("ResolveSecretReference() error exposed secret value: %v", err)
	}
}
