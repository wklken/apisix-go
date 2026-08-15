package store

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	bolt "go.etcd.io/bbolt"
)

func TestMaterializeSecretClonesRedactsAndDestroysValue(t *testing.T) {
	const name = "APISIX_GO_MATERIALIZED_SECRET_TEST"
	const value = "materialized-secret-value"
	t.Setenv(name, value)

	secret, err := MaterializeSecret("$ENV://" + name)
	if err != nil {
		t.Fatalf("MaterializeSecret() error = %v", err)
	}
	if descriptor := secret.Descriptor(); !strings.Contains(descriptor, "$ENV://"+name) ||
		strings.Contains(descriptor, value) {
		t.Fatalf("Descriptor() = %q, want reference/fingerprint without plaintext", descriptor)
	}
	first := secret.Bytes()
	first[0] = 'X'
	if second := secret.Bytes(); !bytes.Equal(second, []byte(value)) {
		t.Fatalf("Bytes() shared caller mutation: %q", second)
	}
	secret.Destroy()
	secret.Destroy()
	if got := secret.Bytes(); got != nil {
		t.Fatalf("Bytes() after Destroy = %q, want nil", got)
	}
}

func TestSecretStoreEventInvalidatesVaultCacheAndTriggersHTTPReload(t *testing.T) {
	secretStore := newConsumerSnapshotStore(t)
	secretStore.vaultSecretCache().Set("vault/test1/foo/key", "stale", time.Minute)
	event := &Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/secrets/vault/test1"),
		Value: []byte(`{"uri":"https://vault.example","prefix":"kv","token":"token"}`),
	}
	if err := secretStore.processEvent(event); err != nil {
		t.Fatalf("processEvent(secret) error = %v", err)
	}
	if secretStore.vaultSecrets != nil {
		t.Fatal("Vault cache survived secret resource replacement")
	}
	if bucket, ok := EventBucket(event); !ok || bucket != "secrets" {
		t.Fatalf("EventBucket(secret) = %q/%t, want secrets/true", bucket, ok)
	}
	if !IsHTTPRouteReloadBucket("secrets") {
		t.Fatal("secrets is not an HTTP route reload bucket")
	}
}

func TestSecretStoreEventCannotBeRepopulatedByInflightVaultFetch(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	secretStore := newConsumerSnapshotStore(t)
	secretStore.vaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":{"passwd":"stale"}}`)),
		}, nil
	})}
	config, err := json.Marshal(vaultSecretConfig{
		URI: "https://vault.example", Prefix: "kv", Token: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := secretStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put([]byte("vault/test1"), config)
	}); err != nil {
		t.Fatal(err)
	}

	oldCache := secretStore.vaultSecretCache()
	result := make(chan error, 1)
	go func() {
		_, resolveErr := secretStore.resolveVaultSecret("vault/test1", "foo/passwd")
		result <- resolveErr
	}()
	<-started
	if err := secretStore.processEvent(&Event{
		Type:  EventTypePut,
		Key:   []byte("/apisix/secrets/vault/test1"),
		Value: config,
	}); err != nil {
		close(release)
		t.Fatal(err)
	}
	newCache := secretStore.vaultSecretCache()
	if newCache == oldCache {
		close(release)
		t.Fatal("secret event retained the prior Vault cache")
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("in-flight Vault fetch error = %v", err)
	}
	if value, ok := newCache.Get("vault/test1/foo/passwd"); ok {
		t.Fatalf("replacement cache was repopulated with stale value %q", value)
	}
}

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

func TestResolveConsumerSecretValuePassesThroughNonReferences(t *testing.T) {
	consumerStore := &Store{}
	value, err := consumerStore.resolveConsumerSecretValue(map[string]any{
		"plain":  "value",
		"count":  3,
		"list":   []any{"a", "b"},
		"nested": map[string]any{"inner": true},
	})
	if err != nil {
		t.Fatalf("resolveConsumerSecretValue() error = %v", err)
	}
	typed := value.(map[string]any)
	if typed["plain"] != "value" || typed["count"] != 3 {
		t.Fatalf("resolved = %#v", typed)
	}
	if _, err := consumerStore.resolveConsumerSecretValue("$env://MISSING_KEY"); err == nil {
		t.Fatal("resolveConsumerSecretValue(missing env secret) error = nil")
	}
}

func TestResolveVaultSecretReusesClientAndCachesSuccess(t *testing.T) {
	var requests atomic.Int32
	var deadlineCalls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		if _, ok := req.Context().Deadline(); ok {
			deadlineCalls.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"data":{"passwd":"bar"}}`)),
		}, nil
	})

	consumerStore := newConsumerSnapshotStore(t)
	consumerStore.vaultClient = &http.Client{Transport: transport}
	config, err := json.Marshal(vaultSecretConfig{
		URI: "http://vault.example.invalid", Prefix: "kv/apisix", Token: "root",
	})
	if err != nil {
		t.Fatalf("encode Vault config: %v", err)
	}
	if err := consumerStore.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put([]byte("vault/test1"), config)
	}); err != nil {
		t.Fatalf("store Vault config: %v", err)
	}

	for i := range 3 {
		got, err := consumerStore.resolveVaultSecret("vault/test1", "foo/passwd")
		if err != nil {
			t.Fatalf("resolveVaultSecret attempt %d: %v", i+1, err)
		}
		if got != "bar" {
			t.Fatalf("resolveVaultSecret attempt %d = %q, want bar", i+1, got)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Vault transport requests = %d, want a single fetch reused by the cache", got)
	}
	if got := deadlineCalls.Load(); got != 1 {
		t.Fatalf("Vault requests with a deadline context = %d, want 1", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
