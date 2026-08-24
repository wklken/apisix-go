package secret

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
)

var (
	ErrCredentialUnavailable     = errors.New("credential unavailable")
	ErrInvalidCapability         = errors.New("invalid secret capability")
	ErrCapabilityScopeMismatch   = errors.New("secret capability scope mismatch")
	ErrInvalidScope              = errors.New("invalid secret scope")
	ErrAttemptAlreadyRegistered  = errors.New("secret attempt already registered")
	errSecretDeclarationInvalid  = errors.New("secret declaration is not admitted")
	errSecretUseCallbackRequired = errors.New("secret use callback is required")
)

// Scope identifies the exact resource and declaration admitted by one
// generation attempt. Domain is deliberately part of the authority key: the
// same resource key may have different bytes in independently published
// domains.
type Scope struct {
	Generation uint64
	Attempt    AttemptID
	Domain     generation.Domain
	Plugin     string
	Resource   generation.ResourceKey
	Source     capability.SecretDeclarationSource
	Field      string
}

type Value struct {
	plaintext string
	digest    [32]byte
}

func (value Value) Use(use func(string) error) error {
	if use == nil {
		return errSecretUseCallbackRequired
	}
	return use(value.plaintext)
}

func (value Value) Digest() [32]byte {
	return value.digest
}

type ScopedResolver interface {
	ResolveScoped(context.Context, Scope, string) (string, error)
}

type ScopedAttemptBroker interface {
	ScopedResolver
	AuthorizeCandidate(
		context.Context,
		AttemptID,
		generation.ApplyTicket,
		generation.PublicationSet,
	) error
	AuthorizeRecovery(
		context.Context,
		AttemptID,
		generation.RevisionSet,
		map[generation.Domain]generation.PublishedGeneration,
	) error
	RevokeAttempt(context.Context, AttemptID) error
}

type AttemptResolver interface {
	ScopedResolver
	Close(context.Context) error
}

type AttemptResolverFactory interface {
	OpenCandidate(
		context.Context,
		AttemptID,
		generation.ApplyTicket,
		generation.PublicationSet,
	) (AttemptResolver, error)
	OpenRecovery(
		context.Context,
		AttemptID,
		generation.RevisionSet,
		map[generation.Domain]generation.PublishedGeneration,
	) (AttemptResolver, error)
}

type AttemptRegistration interface {
	AttemptID() AttemptID
	Materialize(context.Context, Scope, string) (Value, error)
	Close(context.Context) error
}

type Materializer interface {
	RegisterCandidate(
		context.Context,
		generation.ApplyTicket,
		generation.PublicationSet,
	) (AttemptRegistration, error)
	RegisterRecovery(
		context.Context,
		generation.RevisionSet,
		map[generation.Domain]generation.PublishedGeneration,
	) (AttemptRegistration, error)
	DeclarationDigest() [32]byte
}

type materializer struct {
	encryption data_encryption.Service
	resolvers  AttemptResolverFactory
	registry   *attemptRegistry
}

type scopedMaterializer struct {
	resolver ScopedAttemptBroker
	catalog  *capability.SecretDeclarationCatalog
	registry *attemptRegistry
}

// NewMaterializer constructs the in-process materializer. A zero-value
// encryption service or nil resolver factory creates a fail-closed value; it
// never creates a registration that can resolve credentials.
func NewMaterializer(encryption data_encryption.Service, resolvers AttemptResolverFactory) Materializer {
	return &materializer{
		encryption: encryption,
		resolvers:  resolvers,
		registry:   newAttemptRegistry(),
	}
}

// NewScopedMaterializer constructs the worker/scoped materializer. The
// manifest catalog is an explicit dependency so source and field admission is
// identical across the in-process and worker paths.
func NewScopedMaterializer(resolver ScopedAttemptBroker, catalog *capability.SecretDeclarationCatalog) Materializer {
	return &scopedMaterializer{
		resolver: resolver,
		catalog:  catalog,
		registry: newAttemptRegistry(),
	}
}

func (materializer *materializer) DeclarationDigest() [32]byte {
	if materializer == nil || !materializer.encryption.Configured() {
		return [32]byte{}
	}
	return materializer.encryption.DeclarationDigest()
}

func (materializer *scopedMaterializer) DeclarationDigest() [32]byte {
	if materializer == nil || materializer.catalog == nil {
		return [32]byte{}
	}
	return materializer.catalog.Digest()
}

func (materializer *materializer) RegisterCandidate(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptRegistration, error) {
	if materializer == nil ||
		materializer.registry == nil ||
		!materializer.encryption.Configured() ||
		materializer.resolvers == nil {
		return nil, ErrInvalidCapability
	}
	if err := validateCandidateRegistration(ticket, set); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ownedTicket := cloneApplyTicket(ticket)
	ownedSet := clonePublicationSet(set)
	id, err := candidateAttemptIDChecked(ownedTicket, ownedSet)
	if err != nil || id == (AttemptID{}) {
		return nil, ErrInvalidCapability
	}
	allowed := buildCandidateClosureIndex(ownedSet)
	if err := materializer.registry.reserve(id); err != nil {
		return nil, err
	}
	resolver, openErr := materializer.resolvers.OpenCandidate(ctx, id, ownedTicket, ownedSet)
	if openErr != nil || resolver == nil {
		cleanupOpenedAttempt(materializer.registry, id, resolver, ctx)
		return nil, ErrCredentialUnavailable
	}

	registration := newAttemptRegistration(
		id,
		ticket.DesiredRevision,
		allowed,
		func(resolveCtx context.Context, scope Scope, raw string) (string, error) {
			return materializer.resolve(resolver, resolveCtx, scope, raw)
		},
		resolver.Close,
		materializer.registry,
	)
	materializer.registry.activate(id)
	return registration, nil
}

func (materializer *materializer) RegisterRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptRegistration, error) {
	if materializer == nil ||
		materializer.registry == nil ||
		!materializer.encryption.Configured() ||
		materializer.resolvers == nil {
		return nil, ErrInvalidCapability
	}
	if err := validateRecoveryRegistration(revisions, published); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ownedRevisions := revisions
	ownedPublished := clonePublishedGenerations(published)
	id, err := recoveryAttemptIDChecked(ownedRevisions, ownedPublished)
	if err != nil || id == (AttemptID{}) {
		return nil, ErrInvalidCapability
	}
	allowed := buildRecoveryClosureIndex(ownedPublished)
	if err := materializer.registry.reserve(id); err != nil {
		return nil, err
	}
	resolver, openErr := materializer.resolvers.OpenRecovery(ctx, id, ownedRevisions, ownedPublished)
	if openErr != nil || resolver == nil {
		cleanupOpenedAttempt(materializer.registry, id, resolver, ctx)
		return nil, ErrCredentialUnavailable
	}

	registration := newAttemptRegistration(
		id,
		revisions.Desired,
		allowed,
		func(resolveCtx context.Context, scope Scope, raw string) (string, error) {
			return materializer.resolve(resolver, resolveCtx, scope, raw)
		},
		resolver.Close,
		materializer.registry,
	)
	materializer.registry.activate(id)
	return registration, nil
}

func (materializer *materializer) resolve(
	resolver AttemptResolver,
	ctx context.Context,
	scope Scope,
	raw string,
) (string, error) {
	if _, err := materializer.encryption.ValidateDeclaration(scope.Plugin, scope.Source, scope.Field); err != nil {
		return "", errSecretDeclarationInvalid
	}
	if isReference(raw) {
		return resolver.ResolveScoped(ctx, scope, raw)
	}
	resolved, err := materializer.encryption.ResolveDeclared(scope.Plugin, scope.Source, scope.Field, raw)
	if err != nil {
		return "", err
	}
	if isReference(resolved) {
		return resolver.ResolveScoped(ctx, scope, resolved)
	}
	return resolved, nil
}

func (materializer *scopedMaterializer) RegisterCandidate(
	ctx context.Context,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (AttemptRegistration, error) {
	if materializer == nil || materializer.registry == nil || materializer.resolver == nil ||
		materializer.catalog == nil {
		return nil, ErrInvalidCapability
	}
	if err := validateCandidateRegistration(ticket, set); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ownedTicket := cloneApplyTicket(ticket)
	ownedSet := clonePublicationSet(set)
	id, err := candidateAttemptIDChecked(ownedTicket, ownedSet)
	if err != nil || id == (AttemptID{}) {
		return nil, ErrInvalidCapability
	}
	allowed := buildCandidateClosureIndex(ownedSet)
	if err := materializer.registry.reserve(id); err != nil {
		return nil, err
	}
	if err := materializer.resolver.AuthorizeCandidate(ctx, id, ownedTicket, ownedSet); err != nil {
		materializer.registry.release(id)
		return nil, ErrCredentialUnavailable
	}

	resolve := func(resolveCtx context.Context, scope Scope, raw string) (string, error) {
		if _, ok := materializer.catalog.Lookup(scope.Plugin, scope.Source, scope.Field); !ok {
			return "", errSecretDeclarationInvalid
		}
		return materializer.resolver.ResolveScoped(resolveCtx, scope, raw)
	}
	closeAttempt := func(closeCtx context.Context) error {
		return materializer.resolver.RevokeAttempt(closeCtx, id)
	}
	registration := newAttemptRegistration(
		id,
		ticket.DesiredRevision,
		allowed,
		resolve,
		closeAttempt,
		materializer.registry,
	)
	materializer.registry.activate(id)
	return registration, nil
}

func (materializer *scopedMaterializer) RegisterRecovery(
	ctx context.Context,
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) (AttemptRegistration, error) {
	if materializer == nil || materializer.registry == nil || materializer.resolver == nil ||
		materializer.catalog == nil {
		return nil, ErrInvalidCapability
	}
	if err := validateRecoveryRegistration(revisions, published); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ownedPublished := clonePublishedGenerations(published)
	id, err := recoveryAttemptIDChecked(revisions, ownedPublished)
	if err != nil || id == (AttemptID{}) {
		return nil, ErrInvalidCapability
	}
	allowed := buildRecoveryClosureIndex(ownedPublished)
	if err := materializer.registry.reserve(id); err != nil {
		return nil, err
	}
	if err := materializer.resolver.AuthorizeRecovery(ctx, id, revisions, ownedPublished); err != nil {
		materializer.registry.release(id)
		return nil, ErrCredentialUnavailable
	}

	resolve := func(resolveCtx context.Context, scope Scope, raw string) (string, error) {
		if _, ok := materializer.catalog.Lookup(scope.Plugin, scope.Source, scope.Field); !ok {
			return "", errSecretDeclarationInvalid
		}
		return materializer.resolver.ResolveScoped(resolveCtx, scope, raw)
	}
	closeAttempt := func(closeCtx context.Context) error {
		return materializer.resolver.RevokeAttempt(closeCtx, id)
	}
	registration := newAttemptRegistration(id, revisions.Desired, allowed, resolve, closeAttempt, materializer.registry)
	materializer.registry.activate(id)
	return registration, nil
}

type attemptRegistration struct {
	id         AttemptID
	generation uint64
	allowed    closureIndex
	resolve    func(context.Context, Scope, string) (string, error)
	close      func(context.Context) error
	registry   *attemptRegistry

	gate      sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

func newAttemptRegistration(
	id AttemptID,
	generationNumber uint64,
	allowed closureIndex,
	resolve func(context.Context, Scope, string) (string, error),
	closeAttempt func(context.Context) error,
	registry *attemptRegistry,
) *attemptRegistration {
	return &attemptRegistration{
		id:         id,
		generation: generationNumber,
		allowed:    allowed,
		resolve:    resolve,
		close:      closeAttempt,
		registry:   registry,
	}
}

func (registration *attemptRegistration) AttemptID() AttemptID {
	if registration == nil {
		return AttemptID{}
	}
	return registration.id
}

func (registration *attemptRegistration) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if registration == nil {
		return Value{}, ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	registration.gate.RLock()
	defer registration.gate.RUnlock()
	if registration.closed {
		return Value{}, ErrCredentialUnavailable
	}
	if err := validateScope(scope); err != nil {
		return Value{}, err
	}
	if scope.Generation != registration.generation || scope.Attempt != registration.id ||
		!registration.allowed.Contains(scope.Domain, scope.Resource) {
		return Value{}, ErrCapabilityScopeMismatch
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if registration.resolve == nil {
		return Value{}, ErrCredentialUnavailable
	}
	resolved, err := registration.resolve(ctx, scope, raw)
	if err != nil {
		if errors.Is(err, errSecretDeclarationInvalid) {
			return Value{}, ErrInvalidScope
		}
		return Value{}, ErrCredentialUnavailable
	}
	return newValue(resolved), nil
}

func (registration *attemptRegistration) Close(ctx context.Context) error {
	if registration == nil {
		return ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	registration.closeOnce.Do(func() {
		registration.gate.Lock()
		registration.closed = true
		registration.gate.Unlock()

		cleanupErr := error(nil)
		if registration.close != nil {
			cleanupErr = registration.close(context.WithoutCancel(ctx))
		}
		if cleanupErr != nil {
			registration.closeErr = ErrCredentialUnavailable
			registration.registry.quarantine(registration.id)
			return
		}
		registration.registry.release(registration.id)
	})
	return registration.closeErr
}

type GenerationCapability struct {
	generation uint64
	attempt    AttemptRegistration
}

func NewGenerationCapability(attempt AttemptRegistration, generationNumber uint64) (GenerationCapability, error) {
	if attempt == nil || generationNumber == 0 || attempt.AttemptID() == (AttemptID{}) {
		return GenerationCapability{}, ErrInvalidCapability
	}
	return GenerationCapability{generation: generationNumber, attempt: attempt}, nil
}

func (capability GenerationCapability) AttemptID() AttemptID {
	if capability.attempt == nil {
		return AttemptID{}
	}
	return capability.attempt.AttemptID()
}

func (capability GenerationCapability) Generation() uint64 {
	return capability.generation
}

func (capability GenerationCapability) Valid() bool {
	return capability.generation != 0 && capability.AttemptID() != (AttemptID{})
}

// SameAuthority reports whether both capabilities refer to the exact same
// registration and generation. It exposes no registration lifecycle methods
// or internal authority fields and fails closed for non-pointer registrations.
func (capability GenerationCapability) SameAuthority(other GenerationCapability) bool {
	if !capability.Valid() || !other.Valid() || capability.generation != other.generation {
		return false
	}
	left := reflect.ValueOf(capability.attempt)
	right := reflect.ValueOf(other.attempt)
	return left.IsValid() && right.IsValid() && left.Type() == right.Type() &&
		left.Kind() == reflect.Pointer && left.Pointer() == right.Pointer()
}

func (capability GenerationCapability) Materialize(ctx context.Context, scope Scope, raw string) (Value, error) {
	if capability.attempt == nil || capability.generation == 0 {
		return Value{}, ErrInvalidCapability
	}
	if scope.Generation != capability.generation || scope.Attempt != capability.AttemptID() {
		return Value{}, ErrCapabilityScopeMismatch
	}
	return capability.attempt.Materialize(ctx, scope, raw)
}

func validateCandidateRegistration(ticket generation.ApplyTicket, set generation.PublicationSet) error {
	if err := generation.ValidatePublicationSet(ticket, set); err != nil {
		return ErrInvalidCapability
	}
	if len(ticket.RequiredDomains) == 0 {
		return ErrInvalidCapability
	}
	return nil
}

func validateRecoveryRegistration(
	revisions generation.RevisionSet,
	published map[generation.Domain]generation.PublishedGeneration,
) error {
	if len(published) == 0 || generation.ValidateRecoverySet(revisions, published) != nil {
		return ErrInvalidCapability
	}
	return nil
}

type closureIndex map[generation.Domain]map[generation.ResourceKey]struct{}

func (index closureIndex) Contains(domain generation.Domain, resource generation.ResourceKey) bool {
	resources, ok := index[domain]
	if !ok {
		return false
	}
	_, ok = resources[resource]
	return ok
}

func buildCandidateClosureIndex(set generation.PublicationSet) closureIndex {
	index := make(closureIndex, len(set.Domains))
	for domain, candidate := range set.Domains {
		resources := make(map[generation.ResourceKey]struct{})
		for _, decision := range candidate.Decisions {
			if decision.Disposition != generation.DispositionPublished &&
				decision.Disposition != generation.DispositionLastGood {
				continue
			}
			if _, present := candidate.Snapshot.Lookup(decision.Key); present {
				resources[decision.Key] = struct{}{}
			}
		}
		index[domain] = resources
	}
	return index
}

func buildRecoveryClosureIndex(published map[generation.Domain]generation.PublishedGeneration) closureIndex {
	index := make(closureIndex, len(published))
	for domain, value := range published {
		resources := make(map[generation.ResourceKey]struct{})
		for _, decision := range value.Decisions {
			if decision.Disposition != generation.DispositionPublished &&
				decision.Disposition != generation.DispositionLastGood {
				continue
			}
			if _, present := value.Snapshot.Lookup(decision.Key); present {
				resources[decision.Key] = struct{}{}
			}
		}
		index[domain] = resources
	}
	return index
}

func validateScope(scope Scope) error {
	if scope.Generation == 0 || scope.Attempt == (AttemptID{}) ||
		(scope.Domain != generation.DomainHTTP && scope.Domain != generation.DomainStream) ||
		scope.Plugin == "" || scope.Resource.Kind == "" || scope.Resource.ID == "" ||
		(scope.Source != capability.SecretPluginConfig && scope.Source != capability.SecretPluginMetadata &&
			scope.Source != capability.SecretConsumerConfig) ||
		scope.Field == "" {
		return ErrInvalidScope
	}
	return nil
}

func newValue(plaintext string) Value {
	return Value{plaintext: plaintext, digest: sha256.Sum256([]byte(plaintext))}
}

func isReference(value string) bool {
	return strings.HasPrefix(value, "$secret://") || strings.HasPrefix(strings.ToUpper(value), "$ENV://")
}

func normalizeContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type attemptState uint8

const (
	attemptReserved attemptState = iota + 1
	attemptLive
	attemptQuarantined
)

type attemptRegistry struct {
	mu       sync.Mutex
	attempts map[AttemptID]attemptState
}

func cleanupOpenedAttempt(
	registry *attemptRegistry,
	id AttemptID,
	resolver AttemptResolver,
	ctx context.Context,
) {
	if resolver != nil && resolver.Close(context.WithoutCancel(normalizeContext(ctx))) != nil {
		registry.quarantine(id)
		return
	}
	registry.release(id)
}

func newAttemptRegistry() *attemptRegistry {
	return &attemptRegistry{attempts: make(map[AttemptID]attemptState)}
}

func (registry *attemptRegistry) reserve(id AttemptID) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.attempts[id]; exists {
		return ErrAttemptAlreadyRegistered
	}
	registry.attempts[id] = attemptReserved
	return nil
}

func (registry *attemptRegistry) activate(id AttemptID) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.attempts[id] == attemptReserved {
		registry.attempts[id] = attemptLive
	}
}

func (registry *attemptRegistry) release(id AttemptID) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.attempts, id)
}

func (registry *attemptRegistry) quarantine(id AttemptID) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.attempts[id] = attemptQuarantined
}

func cloneApplyTicket(ticket generation.ApplyTicket) generation.ApplyTicket {
	ticket.RequiredDomains = append([]generation.Domain(nil), ticket.RequiredDomains...)
	return ticket
}

func clonePublicationSet(set generation.PublicationSet) generation.PublicationSet {
	owned := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		owned.Domains[domain] = clonePublicationCandidate(candidate)
	}
	return owned
}

func clonePublicationCandidate(candidate generation.PublicationCandidate) generation.PublicationCandidate {
	return generation.PublicationCandidate{
		Artifact:  candidate.Artifact,
		Snapshot:  candidate.Snapshot.Clone(),
		Closure:   append([]generation.ResourceKey(nil), candidate.Closure...),
		Decisions: append([]generation.ResourceDecision(nil), candidate.Decisions...),
	}
}

func clonePublishedGenerations(
	published map[generation.Domain]generation.PublishedGeneration,
) map[generation.Domain]generation.PublishedGeneration {
	owned := make(map[generation.Domain]generation.PublishedGeneration, len(published))
	for domain, value := range published {
		owned[domain] = generation.PublishedGeneration{
			Artifact:  value.Artifact,
			Snapshot:  value.Snapshot.Clone(),
			Closure:   append([]generation.ResourceKey(nil), value.Closure...),
			Decisions: append([]generation.ResourceDecision(nil), value.Decisions...),
		}
	}
	return owned
}
