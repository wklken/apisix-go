package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/secret"
	bolt "go.etcd.io/bbolt"
)

func TestScopedAttemptBrokerKeepsCandidateAndRecoveryAttemptsSeparate(t *testing.T) {
	storage := &Store{dataEncryption: testDataEncryption()}
	ticket, set := scopedBrokerCandidate(t, 9, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}: []byte(`{"id":"route-1"}`),
	})
	candidateID := secret.CandidateAttemptID(ticket, set)
	revisions := generation.RevisionSet{Desired: 9, HTTP: 9}
	published := map[generation.Domain]generation.PublishedGeneration{
		generation.DomainHTTP: generation.PublishedGeneration(set.Domains[generation.DomainHTTP]),
	}
	recoveryID := secret.RecoveryAttemptID(revisions, published)
	if candidateID == recoveryID {
		t.Fatal("candidate and recovery attempt IDs alias")
	}

	if err := storage.AuthorizeCandidate(context.Background(), candidateID, ticket, set); err != nil {
		t.Fatalf("AuthorizeCandidate() error = %v", err)
	}
	if err := storage.AuthorizeCandidate(context.Background(), candidateID, ticket, set); !errors.Is(
		err, secret.ErrAttemptAlreadyRegistered,
	) {
		t.Fatalf("duplicate AuthorizeCandidate() error = %v, want ErrAttemptAlreadyRegistered", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.AuthorizeCandidate(canceled, candidateID, ticket, set); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled AuthorizeCandidate() error = %v, want context.Canceled before duplicate check", err)
	}
	if err := storage.AuthorizeRecovery(context.Background(), recoveryID, revisions, published); err != nil {
		t.Fatalf("AuthorizeRecovery() error = %v", err)
	}

	for name, id := range map[string]secret.AttemptID{"candidate": candidateID, "recovery": recoveryID} {
		t.Run(name, func(t *testing.T) {
			got, err := storage.ResolveScoped(context.Background(), secret.Scope{
				Generation: 9,
				Attempt:    id,
				Domain:     generation.DomainHTTP,
				Plugin:     "key-auth",
				Resource:   generation.ResourceKey{Kind: "routes", ID: "route-1"},
				Source:     capability.SecretPluginConfig,
				Field:      "key",
			}, "literal-value")
			if err != nil || got != "literal-value" {
				t.Fatalf("ResolveScoped() = %q/%v, want literal-value", got, err)
			}
		})
	}
}

func TestScopedAttemptBrokerRejectsAuthorityMismatchesBeforeVault(t *testing.T) {
	var requests atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{"password":"backend-value"}}`))
	}))
	defer vault.Close()

	storage := &Store{dataEncryption: testDataEncryption()}
	id, scope := authorizeScopedVault(t, storage, 11, vaultConfigBytes(t, vault.URL, "root"))
	reference := "$secret://vault/test1/foo/password"
	otherAttempt := id
	otherAttempt[0]++
	tests := map[string]secret.Scope{
		"attempt":    withScope(scope, func(value *secret.Scope) { value.Attempt = otherAttempt }),
		"generation": withScope(scope, func(value *secret.Scope) { value.Generation++ }),
		"domain":     withScope(scope, func(value *secret.Scope) { value.Domain = generation.DomainStream }),
		"resource": withScope(scope, func(value *secret.Scope) {
			value.Resource = generation.ResourceKey{Kind: "routes", ID: "other"}
		}),
		"source": withScope(scope, func(value *secret.Scope) {
			value.Source = capability.SecretPluginMetadata
		}),
		"field": withScope(scope, func(value *secret.Scope) {
			value.Field = "missing"
		}),
	}
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := storage.ResolveScoped(context.Background(), invalid, reference)
			if err == nil {
				t.Fatal("ResolveScoped() error = nil for mismatched authority")
			}
			if strings.Contains(err.Error(), reference) || strings.Contains(err.Error(), "backend-value") {
				t.Fatalf("ResolveScoped() error exposed credential material: %v", err)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests after rejected scopes = %d, want 0", got)
	}
	if err := storage.AuthorizeCandidate(
		context.Background(), secret.AttemptID{1}, generation.ApplyTicket{}, generation.PublicationSet{},
	); err == nil {
		t.Fatal("AuthorizeCandidate(malformed) error = nil")
	}
}

func TestScopedAttemptBrokerUsesRetainedVaultConfigAndConfigDigestCacheIdentity(t *testing.T) {
	var firstRequests atomic.Int32
	first := scopedVaultServer(t, &firstRequests, "first-value")
	defer first.Close()
	var secondRequests atomic.Int32
	second := scopedVaultServer(t, &secondRequests, "second-value")
	defer second.Close()

	storage := &Store{dataEncryption: testDataEncryption()}
	id, scope := authorizeScopedVault(t, storage, 13, vaultConfigBytes(t, first.URL, "root"))
	reference := "$secret://vault/test1/foo/password"
	got, err := storage.ResolveScoped(context.Background(), scope, reference)
	if err != nil || got != "first-value" {
		t.Fatalf("first ResolveScoped() = %q/%v", got, err)
	}

	storage.secretBroker.mu.Lock()
	view := storage.secretBroker.attempts[id]
	storage.secretBroker.mu.Unlock()
	configKey := generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}
	view.gate.Lock()
	oldConfig := view.resources[generation.DomainHTTP][configKey]
	view.resources[generation.DomainHTTP][configKey] = vaultConfigBytes(t, second.URL, "root")
	view.gate.Unlock()
	clear(oldConfig)

	got, err = storage.ResolveScoped(context.Background(), scope, reference)
	if err != nil || got != "second-value" {
		t.Fatalf("second ResolveScoped() = %q/%v, want config-specific fetch", got, err)
	}
	if firstRequests.Load() != 1 || secondRequests.Load() != 1 {
		t.Fatalf("Vault requests = first:%d second:%d, want 1/1", firstRequests.Load(), secondRequests.Load())
	}
}

func TestScopedAttemptBrokerCacheIdentityIncludesResolvedVaultToken(t *testing.T) {
	const tokenEnv = "APISIX_GO_SCOPED_VAULT_TOKEN"
	t.Setenv(tokenEnv, "token-one")
	var requests atomic.Int32
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprintf(w, `{"data":{"password":%q}}`, r.Header.Get("X-Vault-Token"))
	}))
	defer vault.Close()

	storage := &Store{dataEncryption: testDataEncryption()}
	_, scope := authorizeScopedVault(t, storage, 17, vaultConfigBytes(t, vault.URL, "$ENV://"+tokenEnv))
	reference := "$secret://vault/test1/foo/password"
	first, err := storage.ResolveScoped(context.Background(), scope, reference)
	if err != nil || first != "token-one" {
		t.Fatalf("first ResolveScoped() = %q/%v", first, err)
	}
	if err := os.Setenv(tokenEnv, "token-two"); err != nil {
		t.Fatal(err)
	}
	second, err := storage.ResolveScoped(context.Background(), scope, reference)
	if err != nil || second != "token-two" {
		t.Fatalf("second ResolveScoped() = %q/%v, want rotated token", second, err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("Vault requests after token rotation = %d, want 2", got)
	}
}

func TestScopedAttemptBrokerUsesCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	vault := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer vault.Close()
	storage := &Store{dataEncryption: testDataEncryption()}
	_, scope := authorizeScopedVault(t, storage, 19, vaultConfigBytes(t, vault.URL, "root"))
	reference := "$secret://vault/test1/foo/password"

	precanceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.ResolveScoped(precanceled, scope, reference); !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveScoped(precanceled) error = %v, want context.Canceled", err)
	}
	select {
	case <-started:
		t.Fatal("precanceled resolution reached Vault")
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := storage.ResolveScoped(ctx, scope, reference)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ResolveScoped(canceled inflight) error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResolveScoped did not propagate caller cancellation")
	}
}

func TestScopedAttemptBrokerZeroesPartialVaultBodyOnReadError(t *testing.T) {
	const plaintext = "partial-vault-plaintext-must-not-appear"
	reader := &partialErrorReadCloser{payload: []byte(`{"data":{"password":"` + plaintext + `"}}`)}
	storage := &Store{dataEncryption: testDataEncryption()}
	storage.vaultClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       reader,
		}, nil
	})}
	_, scope := authorizeScopedVault(
		t, storage, 21, vaultConfigBytes(t, "http://vault.example.invalid", "root"),
	)
	_, err := storage.ResolveScoped(
		context.Background(), scope, "$secret://vault/test1/foo/password",
	)
	if err == nil {
		t.Fatal("ResolveScoped() error = nil for partial Vault body read")
	}
	if strings.Contains(err.Error(), plaintext) {
		t.Fatalf("ResolveScoped() error exposed partial Vault plaintext: %v", err)
	}
	if len(reader.seen) == 0 || !allZero(reader.seen) {
		t.Fatalf("partial Vault response bytes were not zeroed: %q", reader.seen)
	}
}

func TestScopedAttemptBrokerUsesOnlyAttemptRetainedVaultConfig(t *testing.T) {
	var scopedRequests atomic.Int32
	scopedVault := scopedVaultServer(t, &scopedRequests, "scoped-value")
	defer scopedVault.Close()
	var globalRequests atomic.Int32
	globalVault := scopedVaultServer(t, &globalRequests, "global-value")
	defer globalVault.Close()

	storage := &Store{dataEncryption: testDataEncryption()}
	_, scope := authorizeScopedVault(t, storage, 23, vaultConfigBytes(t, scopedVault.URL, "root"))
	global := newConsumerSnapshotStore(t)
	globalConfig := vaultConfigBytes(t, globalVault.URL, "root")
	if err := global.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("secrets")).Put([]byte("vault/test1"), globalConfig)
	}); err != nil {
		t.Fatal(err)
	}
	previous := ReplaceGlobalStoreForTest(global)
	t.Cleanup(func() { ReplaceGlobalStoreForTest(previous) })

	got, err := storage.ResolveScoped(context.Background(), scope, "$secret://vault/test1/foo/password")
	if err != nil || got != "scoped-value" {
		t.Fatalf("ResolveScoped() = %q/%v, want attempt-retained config", got, err)
	}
	if scopedRequests.Load() != 1 || globalRequests.Load() != 0 {
		t.Fatalf("Vault requests = scoped:%d global:%d, want 1/0", scopedRequests.Load(), globalRequests.Load())
	}
}

func TestScopedAttemptBrokerRequiresRetainedVaultResourceInSameDomain(t *testing.T) {
	var requests atomic.Int32
	vault := scopedVaultServer(t, &requests, "must-not-be-read")
	defer vault.Close()
	storage := &Store{dataEncryption: testDataEncryption()}
	ticket, set := scopedBrokerCandidate(t, 27, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}: []byte(`{"id":"route-1"}`),
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: 27, Attempt: id, Domain: generation.DomainHTTP, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:   capability.SecretPluginConfig, Field: "key",
	}
	_, err := storage.ResolveScoped(context.Background(), scope, "$secret://vault/test1/foo/password")
	if !errors.Is(err, secret.ErrCapabilityScopeMismatch) {
		t.Fatalf("ResolveScoped() error = %v, want missing retained secret authority", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("Vault requests without retained secret resource = %d, want 0", got)
	}
}

func TestScopedAttemptBrokerReferenceAndCiphertextSequencing(t *testing.T) {
	const envName = "APISIX_GO_SCOPED_SEQUENCE_SECRET"
	t.Setenv(envName, "resolved-from-env")
	service := testDataEncryptionWith(true, []string{"qeddd145sfvddff3"})
	storage := &Store{dataEncryption: service}
	ticket, set := scopedBrokerCandidate(t, 28, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}: []byte(`{"id":"route-1"}`),
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatal(err)
	}
	base := secret.Scope{
		Generation: 28, Attempt: id, Domain: generation.DomainHTTP,
		Resource: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:   capability.SecretPluginConfig,
	}

	strict := base
	strict.Plugin = "response-rewrite"
	strict.Field = "body_secret"
	rawReference := "$ENV://" + envName
	got, err := storage.ResolveScoped(context.Background(), strict, rawReference)
	if err != nil || got != "resolved-from-env" {
		t.Fatalf("strict raw-reference ResolveScoped() = %q/%v", got, err)
	}
	if _, err := storage.ResolveScoped(context.Background(), strict, "plaintext-must-not-appear"); err == nil ||
		strings.Contains(err.Error(), "plaintext-must-not-appear") {
		t.Fatalf("strict plaintext error = %v, want redacted rejection", err)
	}
	ciphertext, err := service.EncryptForContext(rawReference, "response-rewrite.body_secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err = storage.ResolveScoped(context.Background(), strict, ciphertext)
	if err != nil || got != "resolved-from-env" {
		t.Fatalf("decrypt-to-reference ResolveScoped() = %q/%v", got, err)
	}
}

func TestScopedAttemptBrokerOwnsAuthorizationInputs(t *testing.T) {
	var requests atomic.Int32
	vault := scopedVaultServer(t, &requests, "owned-value")
	defer vault.Close()
	storage := &Store{dataEncryption: testDataEncryption()}
	ticket, set := scopedBrokerCandidate(t, 30, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}:      []byte(`{"id":"route-1"}`),
		{Kind: "secrets", ID: "vault/test1"}: vaultConfigBytes(t, vault.URL, "root"),
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatal(err)
	}
	candidate := set.Domains[generation.DomainHTTP]
	clear(candidate.Closure)
	clear(candidate.Decisions)
	delete(set.Domains, generation.DomainHTTP)
	ticket.RequiredDomains[0] = generation.DomainStream

	scope := secret.Scope{
		Generation: 30, Attempt: id, Domain: generation.DomainHTTP, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:   capability.SecretPluginConfig, Field: "key",
	}
	got, err := storage.ResolveScoped(context.Background(), scope, "$secret://vault/test1/foo/password")
	if err != nil || got != "owned-value" {
		t.Fatalf("ResolveScoped() after caller mutation = %q/%v", got, err)
	}
}

func TestScopedAttemptBrokerErrorsRedactMalformedReferences(t *testing.T) {
	unsetEnvForTest(t, "must-not-appear")
	const jsonEnv = "APISIX_GO_SCOPED_REDACT_JSON"
	const jsonPlaintext = "environment-json-plaintext-must-not-appear"
	t.Setenv(jsonEnv, `{"field":42,"credential":"`+jsonPlaintext+`"}`)
	storage := &Store{dataEncryption: testDataEncryption()}
	ticket, set := scopedBrokerCandidate(t, 32, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}: []byte(`{"id":"route-1"}`),
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: 32, Attempt: id, Domain: generation.DomainHTTP, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:   capability.SecretPluginConfig, Field: "key",
	}
	for _, raw := range []string{
		"$secret://must-not-appear/malformed",
		"$ENV://must-not-appear/missing",
		"$ENV://" + jsonEnv + "/field",
	} {
		_, err := storage.ResolveScoped(context.Background(), scope, raw)
		if err == nil {
			t.Fatalf("ResolveScoped(%q) error = nil", raw)
		}
		if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "must-not-appear") {
			t.Fatalf("ResolveScoped() error exposed raw reference: %v", err)
		}
		if strings.Contains(err.Error(), jsonPlaintext) {
			t.Fatalf("ResolveScoped() error exposed environment JSON plaintext: %v", err)
		}
	}
}

func TestStoreAttemptSecretCacheZeroesEvictedAndClearedValues(t *testing.T) {
	var cache storeAttemptSecretCache
	firstKey := storeAttemptSecretCacheKey(sha256.Sum256([]byte("first")))
	cache.set(firstKey, "first-plaintext", time.Minute, time.Now())
	cache.mu.Lock()
	firstBytes := cache.entries[firstKey].value
	cache.mu.Unlock()
	for index := 1; index <= vaultSecretCacheCapacity; index++ {
		digest := sha256.Sum256(fmt.Appendf(nil, "key-%d", index))
		cache.set(storeAttemptSecretCacheKey(digest), fmt.Sprintf("value-%d", index), time.Minute, time.Now())
	}
	if !allZero(firstBytes) {
		t.Fatalf("evicted cache plaintext was not zeroed: %q", firstBytes)
	}
	cache.mu.Lock()
	if len(cache.entries) != vaultSecretCacheCapacity {
		cache.mu.Unlock()
		t.Fatalf("cache entries = %d, want %d", len(cache.entries), vaultSecretCacheCapacity)
	}
	var retained []byte
	for _, entry := range cache.entries {
		retained = entry.value
		break
	}
	cache.mu.Unlock()
	cache.clear()
	if !allZero(retained) {
		t.Fatalf("cleared cache plaintext was not zeroed: %q", retained)
	}
	expiryKey := storeAttemptSecretCacheKey(sha256.Sum256([]byte("expiry")))
	cache.set(expiryKey, "expiry-plaintext", 10*time.Millisecond, time.Now())
	cache.mu.Lock()
	expiryBytes := cache.entries[expiryKey].value
	cache.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for !allZero(expiryBytes) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !allZero(expiryBytes) {
		t.Fatalf("expired cache plaintext was not zeroed without a later access: %q", expiryBytes)
	}
}

func TestScopedAttemptBrokerRevokeZeroesRetainedAndCachedBytes(t *testing.T) {
	var requests atomic.Int32
	vault := scopedVaultServer(t, &requests, "cached-plaintext")
	defer vault.Close()
	storage := &Store{dataEncryption: testDataEncryption()}
	id, scope := authorizeScopedVault(t, storage, 33, vaultConfigBytes(t, vault.URL, "root"))
	if _, err := storage.ResolveScoped(
		context.Background(), scope, "$secret://vault/test1/foo/password",
	); err != nil {
		t.Fatal(err)
	}
	storage.secretBroker.mu.Lock()
	view := storage.secretBroker.attempts[id]
	storage.secretBroker.mu.Unlock()
	view.gate.RLock()
	retainedBytes := view.resources[generation.DomainHTTP][generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}]
	view.cache.mu.Lock()
	var cachedBytes []byte
	for _, entry := range view.cache.entries {
		cachedBytes = entry.value
		break
	}
	view.cache.mu.Unlock()
	view.gate.RUnlock()
	if len(cachedBytes) == 0 {
		t.Fatal("ResolveScoped did not populate attempt cache")
	}
	if err := storage.RevokeAttempt(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !allZero(retainedBytes) || !allZero(cachedBytes) {
		t.Fatalf("RevokeAttempt did not zero owned bytes: retained=%q cache=%q", retainedBytes, cachedBytes)
	}
	if view.resources != nil || view.cache.entries != nil {
		t.Fatal("RevokeAttempt retained resource or cache indexes")
	}
}

func TestScopedAttemptBrokerRevokeRemovesLookupAndWaitsForInflight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"resolved-value"}}`))
	}))
	defer vault.Close()
	storage := &Store{dataEncryption: testDataEncryption()}
	ticket, set := scopedBrokerCandidate(t, 29, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}:      []byte(`{"id":"route-1"}`),
		{Kind: "secrets", ID: "vault/test1"}: vaultConfigBytes(t, vault.URL, "root"),
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: 29, Attempt: id, Domain: generation.DomainHTTP, Plugin: "key-auth",
		Resource: generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:   capability.SecretPluginConfig, Field: "key",
	}
	reference := "$secret://vault/test1/foo/password"

	resolveDone := make(chan error, 1)
	go func() {
		_, err := storage.ResolveScoped(context.Background(), scope, reference)
		resolveDone <- err
	}()
	<-started
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- storage.RevokeAttempt(context.Background(), id) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		storage.secretBroker.mu.Lock()
		_, live := storage.secretBroker.attempts[id]
		storage.secretBroker.mu.Unlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("RevokeAttempt did not remove attempt from lookup")
		}
		time.Sleep(time.Millisecond)
	}
	secondRevokeDone := make(chan error, 1)
	go func() { secondRevokeDone <- storage.RevokeAttempt(context.Background(), id) }()
	secondReturnedEarly := false
	var secondRevokeErr error
	select {
	case secondRevokeErr = <-secondRevokeDone:
		secondReturnedEarly = true
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := storage.ResolveScoped(context.Background(), scope, reference); err == nil {
		close(release)
		t.Fatal("new ResolveScoped entered after revoke began")
	}
	duplicateErr := storage.AuthorizeCandidate(context.Background(), id, ticket, set)
	select {
	case err := <-revokeDone:
		close(release)
		t.Fatalf("RevokeAttempt returned before inflight resolution: %v", err)
	default:
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("inflight ResolveScoped() error = %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("RevokeAttempt() error = %v", err)
	}
	if !secondReturnedEarly {
		secondRevokeErr = <-secondRevokeDone
	}
	if secondReturnedEarly {
		if duplicateErr == nil {
			_ = storage.RevokeAttempt(context.Background(), id)
		}
		t.Fatalf("concurrent idempotent RevokeAttempt returned before drain completed: %v", secondRevokeErr)
	}
	if secondRevokeErr != nil {
		t.Fatalf("second concurrent RevokeAttempt() error = %v", secondRevokeErr)
	}
	if !errors.Is(duplicateErr, secret.ErrAttemptAlreadyRegistered) {
		if duplicateErr == nil {
			_ = storage.RevokeAttempt(context.Background(), id)
		}
		t.Fatalf("AuthorizeCandidate during drain error = %v, want ErrAttemptAlreadyRegistered", duplicateErr)
	}
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatalf("AuthorizeCandidate after drain error = %v", err)
	}
	if err := storage.RevokeAttempt(context.Background(), id); err != nil {
		t.Fatalf("cleanup RevokeAttempt() error = %v", err)
	}
}

func TestStoreStopWaitsForAttemptAlreadyDrainingInRevoke(t *testing.T) {
	storage, err := Open(
		filepath.Join(t.TempDir(), "broker-revoke-stop.db"),
		make(chan *Event),
		testDataEncryption(),
	)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"resolved-value"}}`))
	}))
	defer vault.Close()
	id, scope := authorizeScopedVault(t, storage, 39, vaultConfigBytes(t, vault.URL, "root"))
	resolveDone := make(chan error, 1)
	go func() {
		_, resolveErr := storage.ResolveScoped(
			context.Background(), scope, "$secret://vault/test1/foo/password",
		)
		resolveDone <- resolveErr
	}()
	<-started
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- storage.RevokeAttempt(context.Background(), id) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		storage.secretBroker.mu.Lock()
		_, live := storage.secretBroker.attempts[id]
		storage.secretBroker.mu.Unlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("RevokeAttempt did not move the attempt out of live lookup")
		}
		time.Sleep(time.Millisecond)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- storage.Stop() }()
	select {
	case stopErr := <-stopDone:
		close(release)
		<-resolveDone
		<-revokeDone
		t.Fatalf("Store.Stop returned before the draining attempt completed: %v", stopErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("ResolveScoped() error = %v", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("RevokeAttempt() error = %v", err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("Store.Stop() error = %v", err)
	}
}

func TestStoreStopDrainsScopedBrokerBeforeClosingBolt(t *testing.T) {
	storage, err := Open(
		filepath.Join(t.TempDir(), "broker-stop.db"),
		make(chan *Event),
		testDataEncryption(),
	)
	if err != nil {
		t.Fatal(err)
	}
	transport := &closeIdleTransport{base: http.DefaultTransport.(*http.Transport).Clone()}
	storage.vaultClient = &http.Client{Transport: transport}
	started := make(chan struct{})
	release := make(chan struct{})
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(`{"data":{"password":"resolved-value"}}`))
	}))
	defer vault.Close()
	id, scope := authorizeScopedVault(t, storage, 31, vaultConfigBytes(t, vault.URL, "root"))
	storage.secretBroker.mu.Lock()
	view := storage.secretBroker.attempts[id]
	storage.secretBroker.mu.Unlock()
	retainedBytes := view.resources[generation.DomainHTTP][generation.ResourceKey{Kind: "secrets", ID: "vault/test1"}]
	resolveDone := make(chan error, 1)
	go func() {
		_, err := storage.ResolveScoped(
			context.Background(), scope, "$secret://vault/test1/foo/password",
		)
		resolveDone <- err
	}()
	<-started
	stopDone := make(chan error, 1)
	go func() { stopDone <- storage.Stop() }()

	returnedEarly := false
	select {
	case <-stopDone:
		returnedEarly = true
	case <-time.After(100 * time.Millisecond):
		if _, err := storage.GetFromBucket("routes", []byte("route-1")); err != nil {
			close(release)
			t.Fatalf("bbolt closed before broker drain: %v", err)
		}
		ticket, set := scopedBrokerCandidate(t, 32, map[generation.ResourceKey][]byte{
			{Kind: "routes", ID: "route-2"}: []byte(`{"id":"route-2"}`),
		})
		if err := storage.AuthorizeCandidate(
			context.Background(), secret.CandidateAttemptID(ticket, set), ticket, set,
		); err == nil {
			close(release)
			t.Fatal("AuthorizeCandidate entered after Store.Stop began")
		}
	}
	close(release)
	if err := <-resolveDone; err != nil {
		t.Fatalf("inflight ResolveScoped() error = %v", err)
	}
	if !returnedEarly {
		if err := <-stopDone; err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	}
	if returnedEarly {
		t.Fatal("Store.Stop returned before the scoped broker drained")
	}
	if !allZero(retainedBytes) || view.resources != nil {
		t.Fatalf("Store.Stop did not zero retained attempt bytes: %q", retainedBytes)
	}
	if !transport.closed.Load() {
		t.Fatal("Store.Stop did not close idle Vault HTTP connections")
	}
	if err := storage.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if _, err := storage.GetFromBucket("routes", []byte("route-1")); err == nil {
		t.Fatal("bbolt remained open after Store.Stop")
	}
}

func TestJournalOnlyStoreSecretBrokerFailsClosedAndStopsSafely(t *testing.T) {
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "journal.db"), JournalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ticket, set := scopedBrokerCandidate(t, 37, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}: []byte(`{"id":"route-1"}`),
	})
	if err := journal.AuthorizeCandidate(
		context.Background(), secret.CandidateAttemptID(ticket, set), ticket, set,
	); !errors.Is(err, secret.ErrInvalidCapability) {
		t.Fatalf("AuthorizeCandidate() error = %v, want ErrInvalidCapability", err)
	}
	if err := journal.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := journal.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
}

func authorizeScopedVault(
	t *testing.T,
	storage *Store,
	revision uint64,
	config []byte,
) (secret.AttemptID, secret.Scope) {
	t.Helper()
	ticket, set := scopedBrokerCandidate(t, revision, map[generation.ResourceKey][]byte{
		{Kind: "routes", ID: "route-1"}:      []byte(`{"id":"route-1"}`),
		{Kind: "secrets", ID: "vault/test1"}: config,
	})
	id := secret.CandidateAttemptID(ticket, set)
	if err := storage.AuthorizeCandidate(context.Background(), id, ticket, set); err != nil {
		t.Fatalf("AuthorizeCandidate() error = %v", err)
	}
	return id, secret.Scope{
		Generation: revision,
		Attempt:    id,
		Domain:     generation.DomainHTTP,
		Plugin:     "key-auth",
		Resource:   generation.ResourceKey{Kind: "routes", ID: "route-1"},
		Source:     capability.SecretPluginConfig,
		Field:      "key",
	}
}

func vaultConfigBytes(t *testing.T, uri, token string) []byte {
	t.Helper()
	encoded, err := json.Marshal(vaultSecretConfig{
		URI: uri, Prefix: "kv/apisix", Token: token, Timeout: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func scopedVaultServer(t *testing.T, requests *atomic.Int32, value string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v1/kv/apisix/foo" {
			t.Errorf("Vault path = %q, want /v1/kv/apisix/foo", r.URL.Path)
		}
		_, _ = fmt.Fprintf(w, `{"data":{"password":%q}}`, value)
	}))
}

func withScope(value secret.Scope, mutate func(*secret.Scope)) secret.Scope {
	mutate(&value)
	return value
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

type closeIdleTransport struct {
	base   *http.Transport
	closed atomic.Bool
}

type partialErrorReadCloser struct {
	payload []byte
	seen    []byte
	read    bool
}

func (reader *partialErrorReadCloser) Read(target []byte) (int, error) {
	if reader.read {
		return 0, io.EOF
	}
	reader.read = true
	count := copy(target, reader.payload)
	reader.seen = target[:count]
	return count, errors.New("injected partial Vault body read failure")
}

func (*partialErrorReadCloser) Close() error { return nil }

func (transport *closeIdleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport.base.RoundTrip(request)
}

func (transport *closeIdleTransport) CloseIdleConnections() {
	transport.closed.Store(true)
	transport.base.CloseIdleConnections()
}

func scopedBrokerCandidate(
	t *testing.T,
	revision uint64,
	values map[generation.ResourceKey][]byte,
) (generation.ApplyTicket, generation.PublicationSet) {
	t.Helper()
	resources := make([]generation.Resource, 0, len(values))
	closure := make([]generation.ResourceKey, 0, len(values))
	decisions := make([]generation.ResourceDecision, 0, len(values))
	for key, value := range values {
		resources = append(resources, generation.Resource{Key: key, Value: value})
		closure = append(closure, key)
		decisions = append(decisions, generation.ResourceDecision{
			Key: key, Disposition: generation.DispositionPublished, Code: "test-published",
		})
	}
	snapshot, err := generation.NewSnapshot(revision, resources, nil)
	if err != nil {
		t.Fatal(err)
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		Cursor:          generation.ProviderCursor{Provider: "test", Revision: "1"},
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot, Closure: closure, Decisions: decisions,
	}
	return ticket, generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
}
