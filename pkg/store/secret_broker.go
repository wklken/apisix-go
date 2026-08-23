package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/secret"
)

var _ secret.ScopedAttemptBroker = (*Store)(nil)

type storeSecretBroker struct {
	mu       sync.Mutex
	attempts map[secret.AttemptID]*storeSecretAttempt
	draining map[secret.AttemptID]*storeSecretAttempt
	closed   bool
}

type storeSecretAttempt struct {
	gate       sync.RWMutex
	closed     bool
	generation uint64
	resources  map[generation.Domain]map[generation.ResourceKey][]byte
	cache      storeAttemptSecretCache
}

type storeAttemptSecretCacheKey [32]byte

type storeAttemptSecretCacheEntry struct {
	value     []byte
	expiresAt time.Time
	sequence  uint64
	timer     *time.Timer
}

type storeAttemptSecretCache struct {
	mu       sync.Mutex
	entries  map[storeAttemptSecretCacheKey]storeAttemptSecretCacheEntry
	sequence uint64
}

func newStoreSecretBroker() storeSecretBroker {
	return storeSecretBroker{
		attempts: make(map[secret.AttemptID]*storeSecretAttempt),
		draining: make(map[secret.AttemptID]*storeSecretAttempt),
	}
}

func (storage *Store) AuthorizeCandidate(
	ctx context.Context,
	id secret.AttemptID,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) error {
	ctx = storeSecretContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage == nil || !storage.dataEncryption.Configured() ||
		len(ticket.RequiredDomains) == 0 || generation.ValidatePublicationSet(ticket, set) != nil {
		return secret.ErrInvalidCapability
	}
	want := secret.CandidateAttemptID(ticket, set)
	if id == (secret.AttemptID{}) || want == (secret.AttemptID{}) || id != want {
		return secret.ErrInvalidCapability
	}
	view := &storeSecretAttempt{
		generation: ticket.DesiredRevision,
		resources:  retainedCandidateResources(set),
	}
	return storage.authorizeSecretAttempt(ctx, id, view)
}

func (storage *Store) AuthorizeRecovery(
	ctx context.Context,
	id secret.AttemptID,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) error {
	ctx = storeSecretContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage == nil || !storage.dataEncryption.Configured() ||
		revisions.Desired == 0 || len(published) == 0 {
		return secret.ErrInvalidCapability
	}
	for domain, value := range published {
		revision := uint64(0)
		switch domain {
		case generation.DomainHTTP:
			revision = revisions.HTTP
		case generation.DomainStream:
			revision = revisions.Stream
		}
		if revision == 0 || generation.ValidatePublishedGeneration(domain, revision, value) != nil {
			return secret.ErrInvalidCapability
		}
	}
	want := secret.RecoveryAttemptID(revisions, published)
	if id == (secret.AttemptID{}) || want == (secret.AttemptID{}) || id != want {
		return secret.ErrInvalidCapability
	}
	view := &storeSecretAttempt{
		generation: revisions.Desired,
		resources:  retainedRecoveryResources(published),
	}
	return storage.authorizeSecretAttempt(ctx, id, view)
}

func (storage *Store) authorizeSecretAttempt(
	ctx context.Context,
	id secret.AttemptID,
	view *storeSecretAttempt,
) error {
	storage.lifecycleMu.RLock()
	defer storage.lifecycleMu.RUnlock()
	if storage.stopped {
		view.clear()
		return errStoreStopped
	}
	if err := ctx.Err(); err != nil {
		view.clear()
		return err
	}
	storage.secretBroker.mu.Lock()
	defer storage.secretBroker.mu.Unlock()
	if storage.secretBroker.closed {
		view.clear()
		return errStoreStopped
	}
	if storage.secretBroker.attempts == nil {
		storage.secretBroker.attempts = make(map[secret.AttemptID]*storeSecretAttempt)
	}
	if _, exists := storage.secretBroker.attempts[id]; exists {
		view.clear()
		return secret.ErrAttemptAlreadyRegistered
	}
	if _, draining := storage.secretBroker.draining[id]; draining {
		view.clear()
		return secret.ErrAttemptAlreadyRegistered
	}
	storage.secretBroker.attempts[id] = view
	return nil
}

func (storage *Store) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	ctx = storeSecretContext(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if storage == nil || !validStoreSecretScope(scope) || !storage.dataEncryption.Configured() {
		return "", secret.ErrInvalidScope
	}
	storage.secretBroker.mu.Lock()
	view := storage.secretBroker.attempts[scope.Attempt]
	if view == nil {
		storage.secretBroker.mu.Unlock()
		return "", secret.ErrCapabilityScopeMismatch
	}
	view.gate.RLock()
	storage.secretBroker.mu.Unlock()
	defer view.gate.RUnlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if view.closed || view.generation != scope.Generation {
		return "", secret.ErrCapabilityScopeMismatch
	}
	domainResources := view.resources[scope.Domain]
	if _, ok := domainResources[scope.Resource]; !ok {
		return "", secret.ErrCapabilityScopeMismatch
	}
	if _, err := storage.dataEncryption.ValidateDeclaration(scope.Plugin, scope.Source, scope.Field); err != nil {
		return "", secret.ErrInvalidScope
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if isStoreSecretReference(raw) {
		return storage.resolveAttemptSecretReference(ctx, view, scope.Domain, raw)
	}
	resolved, err := storage.dataEncryption.ResolveDeclared(scope.Plugin, scope.Source, scope.Field, raw)
	if err != nil {
		return "", secret.ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if isStoreSecretReference(resolved) {
		return storage.resolveAttemptSecretReference(ctx, view, scope.Domain, resolved)
	}
	return resolved, nil
}

func (storage *Store) RevokeAttempt(ctx context.Context, id secret.AttemptID) error {
	ctx = storeSecretContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if storage == nil || id == (secret.AttemptID{}) {
		return secret.ErrInvalidCapability
	}
	storage.secretBroker.mu.Lock()
	view := storage.secretBroker.attempts[id]
	if view != nil {
		delete(storage.secretBroker.attempts, id)
		if storage.secretBroker.draining == nil {
			storage.secretBroker.draining = make(map[secret.AttemptID]*storeSecretAttempt)
		}
		storage.secretBroker.draining[id] = view
	} else {
		view = storage.secretBroker.draining[id]
	}
	storage.secretBroker.mu.Unlock()
	if view == nil {
		return nil
	}
	view.clear()
	storage.secretBroker.mu.Lock()
	if storage.secretBroker.draining[id] == view {
		delete(storage.secretBroker.draining, id)
	}
	storage.secretBroker.mu.Unlock()
	return nil
}

func retainedCandidateResources(
	set generation.PublicationSet,
) map[generation.Domain]map[generation.ResourceKey][]byte {
	result := make(map[generation.Domain]map[generation.ResourceKey][]byte, len(set.Domains))
	for domain, candidate := range set.Domains {
		result[domain] = retainedResources(candidate.Snapshot, candidate.Decisions)
	}
	return result
}

func retainedRecoveryResources(
	published map[generation.Domain]generation.PublishedGeneration,
) map[generation.Domain]map[generation.ResourceKey][]byte {
	result := make(map[generation.Domain]map[generation.ResourceKey][]byte, len(published))
	for domain, value := range published {
		result[domain] = retainedResources(value.Snapshot, value.Decisions)
	}
	return result
}

func retainedResources(
	snapshot generation.Snapshot,
	decisions []generation.ResourceDecision,
) map[generation.ResourceKey][]byte {
	result := make(map[generation.ResourceKey][]byte)
	for _, decision := range decisions {
		if decision.Disposition != generation.DispositionPublished &&
			decision.Disposition != generation.DispositionLastGood {
			continue
		}
		if raw, ok := snapshot.Lookup(decision.Key); ok {
			// Snapshot.Lookup already returns a caller-owned clone. Transfer that
			// single sensitive copy directly to the attempt for later zeroing.
			result[decision.Key] = raw
		}
	}
	return result
}

func (view *storeSecretAttempt) clear() {
	if view == nil {
		return
	}
	view.gate.Lock()
	defer view.gate.Unlock()
	view.closed = true
	view.clearLocked()
}

func (view *storeSecretAttempt) clearLocked() {
	for _, resources := range view.resources {
		for key, raw := range resources {
			clear(raw)
			delete(resources, key)
		}
	}
	view.resources = nil
	view.cache.clear()
}

func (storage *Store) resolveAttemptSecretReference(
	ctx context.Context,
	view *storeSecretAttempt,
	domain generation.Domain,
	reference string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if hasEnvironmentSecretPrefix(reference) {
		return resolveAttemptEnvironmentSecret(ctx, reference)
	}
	if !strings.HasPrefix(reference, managedSecretPrefix) {
		return "", secret.ErrCredentialUnavailable
	}
	parts := strings.SplitN(strings.TrimPrefix(reference, managedSecretPrefix), "/", 3)
	if len(parts) != 3 || parts[0] != "vault" || parts[1] == "" || parts[2] == "" {
		return "", secret.ErrCredentialUnavailable
	}
	return storage.resolveAttemptVaultSecret(ctx, view, domain, "vault/"+parts[1], parts[2])
}

func (storage *Store) resolveAttemptVaultSecret(
	ctx context.Context,
	view *storeSecretAttempt,
	domain generation.Domain,
	id string,
	key string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	configBytes, ok := view.resources[domain][generation.ResourceKey{Kind: "secrets", ID: id}]
	if !ok {
		return "", secret.ErrCapabilityScopeMismatch
	}
	var config vaultSecretConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return "", secret.ErrCredentialUnavailable
	}
	if config.URI == "" || config.Prefix == "" || config.Token == "" {
		return "", secret.ErrCredentialUnavailable
	}
	lastSlash := strings.LastIndexByte(key, '/')
	if lastSlash <= 0 || lastSlash == len(key)-1 {
		return "", secret.ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	token := config.Token
	if hasEnvironmentSecretPrefix(token) {
		resolved, err := resolveAttemptEnvironmentSecret(ctx, token)
		if err != nil {
			return "", err
		}
		token = resolved
	}
	cacheKey := newStoreAttemptSecretCacheKey(configBytes, token, id, key)
	if cached, ok := view.cache.get(cacheKey, time.Now()); ok {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return cached, nil
	}

	endpoint, err := url.Parse(strings.TrimRight(config.URI, "/"))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", secret.ErrCredentialUnavailable
	}
	endpoint.Path = path.Join(endpoint.Path, "/v1", config.Prefix, key[:lastSlash])
	timeout := time.Duration(config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = defaultVaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", secret.ErrCredentialUnavailable
	}
	request.Header.Set("X-Vault-Token", token)
	if config.Namespace != "" {
		request.Header.Set("X-Vault-Namespace", config.Namespace)
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	response, err := storage.vaultHTTPClient().Do(request)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", secret.ErrCredentialUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	defer clear(body)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", secret.ErrCredentialUnavailable
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	if len(body) > 1<<20 || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", secret.ErrCredentialUnavailable
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", secret.ErrCredentialUnavailable
	}
	field := key[lastSlash+1:]
	value, ok := payload.Data[field].(string)
	if !ok {
		if nested, nestedOK := payload.Data["data"].(map[string]any); nestedOK {
			value, ok = nested[field].(string)
		}
	}
	if !ok {
		return "", secret.ErrCredentialUnavailable
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	view.cache.set(cacheKey, value, vaultSecretCacheTTL, time.Now())
	return value, nil
}

func resolveAttemptEnvironmentSecret(ctx context.Context, reference string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parts := strings.Split(reference[len(environmentSecretPrefix):], "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", secret.ErrCredentialUnavailable
	}
	value, ok := os.LookupEnv(parts[0])
	if !ok {
		return "", secret.ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(parts) == 1 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return value, nil
	}
	var document any
	encoded := []byte(value)
	defer clear(encoded)
	if err := json.Unmarshal(encoded, &document); err != nil {
		return "", secret.ErrCredentialUnavailable
	}
	current := document
	for _, key := range parts[1:] {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		object, objectOK := current.(map[string]any)
		if !objectOK {
			return "", secret.ErrCredentialUnavailable
		}
		current, ok = object[key]
		if !ok {
			return "", secret.ErrCredentialUnavailable
		}
	}
	resolved, ok := current.(string)
	if !ok {
		return "", secret.ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return resolved, nil
}

func newStoreAttemptSecretCacheKey(config []byte, token, id, key string) storeAttemptSecretCacheKey {
	hasher := sha256.New()
	for _, digest := range [][32]byte{
		sha256.Sum256(config), sha256.Sum256([]byte(token)), sha256.Sum256([]byte(id)), sha256.Sum256([]byte(key)),
	} {
		_, _ = hasher.Write(digest[:])
	}
	var result storeAttemptSecretCacheKey
	copy(result[:], hasher.Sum(nil))
	return result
}

func (cache *storeAttemptSecretCache) get(
	key storeAttemptSecretCacheKey,
	now time.Time,
) (string, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		clear(entry.value)
		delete(cache.entries, key)
		return "", false
	}
	return string(bytes.Clone(entry.value)), true
}

func (cache *storeAttemptSecretCache) set(
	key storeAttemptSecretCacheKey,
	value string,
	ttl time.Duration,
	now time.Time,
) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if ttl <= 0 {
		return
	}
	if cache.entries == nil {
		cache.entries = make(map[storeAttemptSecretCacheKey]storeAttemptSecretCacheEntry)
	}
	for existingKey, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			if entry.timer != nil {
				entry.timer.Stop()
			}
			clear(entry.value)
			delete(cache.entries, existingKey)
		}
	}
	if previous, exists := cache.entries[key]; exists {
		if previous.timer != nil {
			previous.timer.Stop()
		}
		clear(previous.value)
	}
	cache.sequence++
	sequence := cache.sequence
	entry := storeAttemptSecretCacheEntry{
		value: []byte(value), expiresAt: now.Add(ttl), sequence: cache.sequence,
	}
	// An expiry timer zeroes plaintext even when no later cache access occurs.
	entry.timer = time.AfterFunc(ttl, func() { cache.expire(key, sequence) })
	cache.entries[key] = entry
	for len(cache.entries) > vaultSecretCacheCapacity {
		var oldestKey storeAttemptSecretCacheKey
		oldestSequence := uint64(^uint64(0))
		for candidateKey, entry := range cache.entries {
			if entry.sequence < oldestSequence {
				oldestKey = candidateKey
				oldestSequence = entry.sequence
			}
		}
		entry := cache.entries[oldestKey]
		if entry.timer != nil {
			entry.timer.Stop()
		}
		clear(entry.value)
		delete(cache.entries, oldestKey)
	}
}

func (cache *storeAttemptSecretCache) expire(key storeAttemptSecretCacheKey, sequence uint64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok || entry.sequence != sequence {
		return
	}
	clear(entry.value)
	delete(cache.entries, key)
}

func (cache *storeAttemptSecretCache) clear() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for key, entry := range cache.entries {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		clear(entry.value)
		delete(cache.entries, key)
	}
	cache.entries = nil
}

func (storage *Store) closeSecretBroker() {
	storage.secretBroker.mu.Lock()
	storage.secretBroker.closed = true
	if storage.secretBroker.draining == nil {
		storage.secretBroker.draining = make(map[secret.AttemptID]*storeSecretAttempt)
	}
	for id, view := range storage.secretBroker.attempts {
		storage.secretBroker.draining[id] = view
		delete(storage.secretBroker.attempts, id)
	}
	draining := make(map[secret.AttemptID]*storeSecretAttempt, len(storage.secretBroker.draining))
	maps.Copy(draining, storage.secretBroker.draining)
	storage.secretBroker.mu.Unlock()
	for _, view := range draining {
		view.clear()
	}
	storage.secretBroker.mu.Lock()
	for id, view := range draining {
		if storage.secretBroker.draining[id] == view {
			delete(storage.secretBroker.draining, id)
		}
	}
	storage.secretBroker.mu.Unlock()
	storage.vaultHTTPClient().CloseIdleConnections()
}

func validStoreSecretScope(scope secret.Scope) bool {
	return scope.Generation != 0 && scope.Attempt != (secret.AttemptID{}) &&
		(scope.Domain == generation.DomainHTTP || scope.Domain == generation.DomainStream) &&
		scope.Plugin != "" && scope.Resource.Kind != "" && scope.Resource.ID != "" &&
		(scope.Source == capability.SecretPluginConfig || scope.Source == capability.SecretPluginMetadata ||
			scope.Source == capability.SecretConsumerConfig) &&
		scope.Field != ""
}

func isStoreSecretReference(value string) bool {
	return strings.HasPrefix(value, managedSecretPrefix) ||
		(len(value) >= len(environmentSecretPrefix) &&
			strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix))
}

func storeSecretContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func hasEnvironmentSecretPrefix(value string) bool {
	return len(value) >= len(environmentSecretPrefix) &&
		strings.EqualFold(value[:len(environmentSecretPrefix)], environmentSecretPrefix)
}
