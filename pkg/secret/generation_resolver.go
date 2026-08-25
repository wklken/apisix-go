package secret

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
)

const (
	generationEnvironmentSecretPrefix  = "$ENV://"
	generationManagedSecretPrefix      = "$secret://"
	generationVaultSecretCacheTTL      = 60 * time.Second
	generationVaultSecretCacheCapacity = 1024
	generationDefaultVaultTimeout      = 5 * time.Second
)

// GenerationSecretResolver owns all in-process secret attempts for immutable
// candidate and recovery publications. It deliberately has no Store or
// snapshot lookup dependency: every resource byte is supplied by the caller's
// validated publication closure.
type GenerationSecretResolver struct {
	mu         sync.Mutex
	encryption data_encryption.Service
	client     *http.Client
	attempts   map[AttemptID]*generationSecretAttempt
	draining   map[AttemptID]*generationSecretAttempt
	closed     bool
	closeOnce  sync.Once
	closeErr   error
}

type generationSecretAttempt struct {
	resolver   *GenerationSecretResolver
	id         AttemptID
	generation uint64
	resources  map[generation.Domain]map[generation.ResourceKey][]byte
	cache      generationAttemptSecretCache

	gate      sync.RWMutex
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

type generationAttemptSecretCacheKey [32]byte

type generationAttemptSecretCacheEntry struct {
	value     []byte
	expiresAt time.Time
	sequence  uint64
	timer     *time.Timer
}

type generationAttemptSecretCache struct {
	mu       sync.Mutex
	entries  map[generationAttemptSecretCacheKey]generationAttemptSecretCacheEntry
	sequence uint64
}

type generationVaultSecretConfig struct {
	URI       string `json:"uri"`
	Prefix    string `json:"prefix"`
	Token     string `json:"token"`
	Namespace string `json:"namespace,omitempty"`
	Timeout   int    `json:"timeout,omitempty"`
}

var (
	_ AttemptResolverFactory = (*GenerationSecretResolver)(nil)
	_ AttemptResolver        = (*generationSecretAttempt)(nil)
)

// NewGenerationSecretResolver constructs the immutable-generation resolver.
// The resolver owns its HTTP client and closes its idle connections after all
// attempts have released their retained bytes.
func NewGenerationSecretResolver(
	encryption data_encryption.Service,
) (*GenerationSecretResolver, error) {
	return newGenerationSecretResolver(encryption, nil)
}

// newGenerationSecretResolver transfers ownership of client to the resolver.
// It is package-private so tests can inject a transport without widening the
// public constructor's API.
func newGenerationSecretResolver(
	encryption data_encryption.Service,
	client *http.Client,
) (*GenerationSecretResolver, error) {
	if !encryption.Configured() {
		return nil, ErrInvalidCapability
	}
	if client == nil {
		client = &http.Client{}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &GenerationSecretResolver{
		encryption: encryption,
		client:     client,
		attempts:   make(map[AttemptID]*generationSecretAttempt),
		draining:   make(map[AttemptID]*generationSecretAttempt),
	}, nil
}

func (resolver *GenerationSecretResolver) OpenCandidate(
	ctx context.Context,
	id AttemptID,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptResolver, error) {
	if resolver == nil || !resolver.encryption.Configured() {
		return nil, ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := generation.ValidatePublicationSet(ticket, set); err != nil {
		return nil, ErrInvalidCapability
	}
	ownedTicket := cloneApplyTicket(ticket)
	ownedSet := clonePublicationSet(set)
	want := CandidateAttemptID(ownedTicket, ownedSet)
	if !equalGenerationAttemptID(id, want) {
		return nil, ErrInvalidCapability
	}
	resources := make(map[generation.Domain]map[generation.ResourceKey][]byte, len(ownedSet.Domains))
	for domain, candidate := range ownedSet.Domains {
		indexed, err := indexGenerationCandidateResources(candidate)
		if err != nil {
			clearGenerationResourceIndex(resources)
			return nil, ErrInvalidCapability
		}
		resources[domain] = indexed
	}
	attempt := &generationSecretAttempt{
		resolver:   resolver,
		id:         id,
		generation: ownedTicket.DesiredRevision,
		resources:  resources,
		cache:      newGenerationAttemptSecretCache(),
	}
	if err := resolver.registerGenerationAttempt(ctx, attempt); err != nil {
		attempt.clearOwnedBytes()
		return nil, err
	}
	return attempt, nil
}

func (resolver *GenerationSecretResolver) OpenRecovery(
	ctx context.Context,
	id AttemptID,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptResolver, error) {
	if resolver == nil || !resolver.encryption.Configured() {
		return nil, ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := generation.ValidateRecoverySet(revisions, published); err != nil {
		return nil, ErrInvalidCapability
	}
	ownedPublished := clonePublishedGenerations(published)
	want := RecoveryAttemptID(revisions, ownedPublished)
	if !equalGenerationAttemptID(id, want) {
		return nil, ErrInvalidCapability
	}
	resources := make(map[generation.Domain]map[generation.ResourceKey][]byte, len(ownedPublished))
	for domain, value := range ownedPublished {
		indexed, err := indexGenerationPublishedResources(value)
		if err != nil {
			clearGenerationResourceIndex(resources)
			return nil, ErrInvalidCapability
		}
		resources[domain] = indexed
	}
	attempt := &generationSecretAttempt{
		resolver:   resolver,
		id:         id,
		generation: revisions.Desired,
		resources:  resources,
		cache:      newGenerationAttemptSecretCache(),
	}
	if err := resolver.registerGenerationAttempt(ctx, attempt); err != nil {
		attempt.clearOwnedBytes()
		return nil, err
	}
	return attempt, nil
}

func (resolver *GenerationSecretResolver) registerGenerationAttempt(
	ctx context.Context,
	attempt *generationSecretAttempt,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.closed {
		return ErrCredentialUnavailable
	}
	if _, exists := resolver.attempts[attempt.id]; exists {
		return ErrAttemptAlreadyRegistered
	}
	if _, exists := resolver.draining[attempt.id]; exists {
		return ErrAttemptAlreadyRegistered
	}
	resolver.attempts[attempt.id] = attempt
	return nil
}

func equalGenerationAttemptID(left, right AttemptID) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1 && right != (AttemptID{})
}

func indexGenerationCandidateResources(
	candidate generation.PublicationCandidate,
) (map[generation.ResourceKey][]byte, error) {
	return indexGenerationResources(candidate.Snapshot, candidate.Decisions)
}

func indexGenerationPublishedResources(
	published generation.PublishedGeneration,
) (map[generation.ResourceKey][]byte, error) {
	return indexGenerationResources(published.Snapshot, published.Decisions)
}

func indexGenerationResources(
	snapshot generation.Snapshot,
	decisions []generation.ResourceDecision,
) (map[generation.ResourceKey][]byte, error) {
	resources := make(map[generation.ResourceKey][]byte)
	for _, decision := range decisions {
		if decision.Disposition != generation.DispositionPublished &&
			decision.Disposition != generation.DispositionLastGood {
			continue
		}
		raw, ok := snapshot.Lookup(decision.Key)
		if !ok {
			clearGenerationResourceIndex(map[generation.Domain]map[generation.ResourceKey][]byte{
				generation.DomainHTTP: resources,
			})
			return nil, ErrInvalidCapability
		}
		resources[decision.Key] = raw
	}
	return resources, nil
}

func clearGenerationResourceIndex(
	resources map[generation.Domain]map[generation.ResourceKey][]byte,
) {
	for domain, values := range resources {
		for key, raw := range values {
			clear(raw)
			delete(values, key)
		}
		delete(resources, domain)
	}
}

func (attempt *generationSecretAttempt) ResolveScoped(
	ctx context.Context,
	scope Scope,
	raw string,
) (string, error) {
	if attempt == nil || attempt.resolver == nil {
		return "", ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateScope(scope); err != nil {
		return "", err
	}
	attempt.resolver.mu.Lock()
	current := attempt.resolver.attempts[scope.Attempt]
	attempt.resolver.mu.Unlock()
	if current != attempt {
		if attempt.closed.Load() {
			return "", ErrCredentialUnavailable
		}
		return "", ErrCapabilityScopeMismatch
	}
	attempt.gate.RLock()
	defer attempt.gate.RUnlock()
	if attempt.closed.Load() || attempt.generation != scope.Generation {
		return "", ErrCapabilityScopeMismatch
	}
	domainResources := attempt.resources[scope.Domain]
	if _, ok := domainResources[scope.Resource]; !ok {
		return "", ErrCapabilityScopeMismatch
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := attempt.resolver.encryption.ValidateDeclaration(scope.Plugin, scope.Source, scope.Field); err != nil {
		return "", ErrInvalidScope
	}
	if isReference(raw) {
		return attempt.resolveReference(ctx, scope.Domain, raw)
	}
	resolved, err := attempt.resolver.encryption.ResolveDeclared(scope.Plugin, scope.Source, scope.Field, raw)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", ErrCredentialUnavailable
	}
	if isReference(resolved) {
		return attempt.resolveReference(ctx, scope.Domain, resolved)
	}
	return resolved, nil
}

func (attempt *generationSecretAttempt) resolveReference(
	ctx context.Context,
	domain generation.Domain,
	reference string,
) (string, error) {
	if hasGenerationEnvironmentPrefix(reference) {
		return resolveGenerationEnvironmentSecret(ctx, reference)
	}
	if !strings.HasPrefix(reference, generationManagedSecretPrefix) {
		return "", ErrCredentialUnavailable
	}
	parts := strings.SplitN(strings.TrimPrefix(reference, generationManagedSecretPrefix), "/", 3)
	if len(parts) != 3 || parts[0] != "vault" || parts[1] == "" || parts[2] == "" {
		return "", ErrCredentialUnavailable
	}
	lastSlash := strings.LastIndexByte(parts[2], '/')
	if lastSlash <= 0 || lastSlash == len(parts[2])-1 {
		return "", ErrCredentialUnavailable
	}
	resourceKey := generation.ResourceKey{Kind: "secrets", ID: "vault/" + parts[1]}
	configBytes, ok := attempt.resources[domain][resourceKey]
	if !ok {
		return "", ErrCapabilityScopeMismatch
	}
	return attempt.resolveVault(ctx, resourceKey.ID, parts[2], configBytes)
}

func resolveGenerationEnvironmentSecret(ctx context.Context, reference string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parts := strings.Split(reference[len(generationEnvironmentSecretPrefix):], "/")
	if len(parts) == 0 || parts[0] == "" {
		return "", ErrCredentialUnavailable
	}
	value, ok := os.LookupEnv(parts[0])
	if !ok {
		return "", ErrCredentialUnavailable
	}
	if len(parts) == 1 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return value, nil
	}
	encoded := []byte(value)
	defer clear(encoded)
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		return "", ErrCredentialUnavailable
	}
	current := document
	for _, key := range parts[1:] {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		object, ok := current.(map[string]any)
		if !ok {
			return "", ErrCredentialUnavailable
		}
		current, ok = object[key]
		if !ok {
			return "", ErrCredentialUnavailable
		}
	}
	resolved, ok := current.(string)
	if !ok {
		return "", ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return resolved, nil
}

func hasGenerationEnvironmentPrefix(value string) bool {
	return len(value) >= len(generationEnvironmentSecretPrefix) &&
		strings.EqualFold(value[:len(generationEnvironmentSecretPrefix)], generationEnvironmentSecretPrefix)
}

func (attempt *generationSecretAttempt) resolveVault(
	ctx context.Context,
	id string,
	key string,
	configBytes []byte,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var config generationVaultSecretConfig
	if err := json.Unmarshal(configBytes, &config); err != nil ||
		config.URI == "" || config.Prefix == "" || config.Token == "" {
		return "", ErrCredentialUnavailable
	}
	token := config.Token
	if hasGenerationEnvironmentPrefix(token) {
		resolved, err := resolveGenerationEnvironmentSecret(ctx, token)
		if err != nil {
			return "", err
		}
		token = resolved
	}
	cacheKey := newGenerationAttemptSecretCacheKey(configBytes, token, id, key)
	if cached, ok := attempt.cache.get(cacheKey, time.Now()); ok {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return cached, nil
	}
	endpoint, err := url.Parse(strings.TrimRight(config.URI, "/"))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return "", ErrCredentialUnavailable
	}
	lastSlash := strings.LastIndexByte(key, '/')
	if lastSlash <= 0 || lastSlash == len(key)-1 {
		return "", ErrCredentialUnavailable
	}
	endpoint.Path = path.Join(endpoint.Path, "/v1", config.Prefix, key[:lastSlash])
	timeout := time.Duration(config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = generationDefaultVaultTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	request.Header.Set("X-Vault-Token", token)
	if config.Namespace != "" {
		request.Header.Set("X-Vault-Namespace", config.Namespace)
	}
	response, err := attempt.resolver.client.Do(request)
	if err != nil {
		if contextErr := requestCtx.Err(); contextErr != nil {
			return "", contextErr
		}
		return "", ErrCredentialUnavailable
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
		return "", ErrCredentialUnavailable
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	if len(body) > 1<<20 || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", ErrCredentialUnavailable
	}
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ErrCredentialUnavailable
	}
	field := key[lastSlash+1:]
	value, ok := payload.Data[field].(string)
	if !ok {
		if nested, nestedOK := payload.Data["data"].(map[string]any); nestedOK {
			value, ok = nested[field].(string)
		}
	}
	if !ok {
		return "", ErrCredentialUnavailable
	}
	if err := requestCtx.Err(); err != nil {
		return "", err
	}
	attempt.cache.set(cacheKey, value, generationVaultSecretCacheTTL, time.Now())
	return value, nil
}

func newGenerationAttemptSecretCache() generationAttemptSecretCache {
	return generationAttemptSecretCache{
		entries: make(map[generationAttemptSecretCacheKey]generationAttemptSecretCacheEntry),
	}
}

func newGenerationAttemptSecretCacheKey(config []byte, token, id, key string) generationAttemptSecretCacheKey {
	hasher := sha256.New()
	for _, digest := range [][32]byte{
		sha256.Sum256(config),
		sha256.Sum256([]byte(token)),
		sha256.Sum256([]byte(id)),
		sha256.Sum256([]byte(key)),
	} {
		_, _ = hasher.Write(digest[:])
	}
	var result generationAttemptSecretCacheKey
	copy(result[:], hasher.Sum(nil))
	return result
}

func (cache *generationAttemptSecretCache) get(
	key generationAttemptSecretCacheKey,
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

func (cache *generationAttemptSecretCache) set(
	key generationAttemptSecretCacheKey,
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
		cache.entries = make(map[generationAttemptSecretCacheKey]generationAttemptSecretCacheEntry)
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
	entry := generationAttemptSecretCacheEntry{
		value:     []byte(value),
		expiresAt: now.Add(ttl),
		sequence:  sequence,
	}
	entry.timer = time.AfterFunc(ttl, func() { cache.expire(key, sequence) })
	cache.entries[key] = entry
	for len(cache.entries) > generationVaultSecretCacheCapacity {
		var oldestKey generationAttemptSecretCacheKey
		oldestSequence := ^uint64(0)
		for candidateKey, candidateEntry := range cache.entries {
			if candidateEntry.sequence < oldestSequence {
				oldestKey = candidateKey
				oldestSequence = candidateEntry.sequence
			}
		}
		oldest := cache.entries[oldestKey]
		if oldest.timer != nil {
			oldest.timer.Stop()
		}
		clear(oldest.value)
		delete(cache.entries, oldestKey)
	}
}

func (cache *generationAttemptSecretCache) expire(
	key generationAttemptSecretCacheKey,
	sequence uint64,
) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok || entry.sequence != sequence {
		return
	}
	clear(entry.value)
	delete(cache.entries, key)
}

func (cache *generationAttemptSecretCache) clear() {
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

func (attempt *generationSecretAttempt) Close(ctx context.Context) error {
	if attempt == nil || attempt.resolver == nil {
		return ErrInvalidCapability
	}
	attempt.closeOnce.Do(func() {
		attempt.closeErr = attempt.resolver.closeGenerationAttempt(attempt, ctx)
	})
	return attempt.closeErr
}

func (resolver *GenerationSecretResolver) closeGenerationAttempt(
	attempt *generationSecretAttempt,
	ctx context.Context,
) error {
	resolver.mu.Lock()
	if current, ok := resolver.attempts[attempt.id]; ok && current == attempt {
		delete(resolver.attempts, attempt.id)
	}
	if current, ok := resolver.draining[attempt.id]; !ok || current == attempt {
		resolver.draining[attempt.id] = attempt
	}
	resolver.mu.Unlock()

	attempt.closed.Store(true)
	attempt.gate.Lock()
	attempt.clearOwnedBytes()
	attempt.gate.Unlock()

	resolver.mu.Lock()
	if current, ok := resolver.draining[attempt.id]; ok && current == attempt {
		delete(resolver.draining, attempt.id)
	}
	resolver.mu.Unlock()
	return nil
}

func (attempt *generationSecretAttempt) clearOwnedBytes() {
	for domain, resources := range attempt.resources {
		clearGenerationResourceMap(resources)
		delete(attempt.resources, domain)
	}
	attempt.resources = nil
	attempt.cache.clear()
}

func clearGenerationResourceMap(resources map[generation.ResourceKey][]byte) {
	for key, raw := range resources {
		clear(raw)
		delete(resources, key)
	}
}

func (resolver *GenerationSecretResolver) Close(ctx context.Context) error {
	if resolver == nil {
		return ErrInvalidCapability
	}
	resolver.closeOnce.Do(func() {
		resolver.mu.Lock()
		resolver.closed = true
		attempts := make(map[AttemptID]*generationSecretAttempt, len(resolver.attempts)+len(resolver.draining))
		for id, attempt := range resolver.attempts {
			attempts[id] = attempt
			resolver.draining[id] = attempt
			delete(resolver.attempts, id)
		}
		maps.Copy(attempts, resolver.draining)
		resolver.mu.Unlock()

		cleanupCtx := context.WithoutCancel(normalizeContext(ctx))
		var cleanupErrors []error
		for _, attempt := range attempts {
			if err := attempt.Close(cleanupCtx); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		resolver.client.CloseIdleConnections()
		resolver.closeErr = errors.Join(cleanupErrors...)
	})
	return resolver.closeErr
}
