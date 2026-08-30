package secret

import (
	"context"
	"crypto/sha256"
	"errors"
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
	errSecretUseCallbackRequired = errors.New("secret use callback is required")
)

// Scope identifies one declared secret field in an immutable runtime
// generation. The compiler supplies every field except Field; plugins may only
// select a field declared for their own factory and source.
type Scope struct {
	Generation uint64
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

func (value Value) Digest() [32]byte { return value.digest }

// GenerationResolver resolves external references against the exact resource
// bytes owned by one generation and releases those bytes on Close.
type GenerationResolver interface {
	ResolveReference(context.Context, Scope, string) (string, error)
	Close(context.Context) error
}

type GenerationResolverFactory interface {
	// OpenGeneration transfers resolver ownership to the caller only when it
	// returns a nil error. On error, implementations return nil after releasing
	// any partial resources.
	OpenGeneration(context.Context, uint64, generation.PublicationSet) (GenerationResolver, error)
}

// GenerationMaterialization is the compiler-owned lifecycle handle. Runtime
// plugins receive only its read-only Secrets view.
type GenerationMaterialization interface {
	Secrets() GenerationSecrets
	Close(context.Context) error
}

type Materializer interface {
	PrepareGeneration(context.Context, generation.PublicationSet) (GenerationMaterialization, error)
	DeclarationDigest() [32]byte
}

type materializer struct {
	encryption data_encryption.Service
	resolvers  GenerationResolverFactory
}

// NewMaterializer constructs a generation-owned secret materializer. A zero
// encryption service or nil resolver factory stays fail closed.
func NewMaterializer(encryption data_encryption.Service, resolvers GenerationResolverFactory) Materializer {
	return &materializer{encryption: encryption, resolvers: resolvers}
}

func (materializer *materializer) DeclarationDigest() [32]byte {
	if materializer == nil || !materializer.encryption.Configured() {
		return [32]byte{}
	}
	return materializer.encryption.DeclarationDigest()
}

func (materializer *materializer) PrepareGeneration(
	ctx context.Context,
	set generation.PublicationSet,
) (GenerationMaterialization, error) {
	if materializer == nil || !materializer.encryption.Configured() || materializer.resolvers == nil {
		return nil, ErrInvalidCapability
	}
	if err := validateGenerationPublication(set); err != nil {
		return nil, err
	}
	ctx = normalizeContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	owned := clonePublicationSet(set)
	resolver, err := materializer.resolvers.OpenGeneration(ctx, set.DesiredRevision, owned)
	if err != nil || resolver == nil {
		if resolver != nil {
			_ = resolver.Close(context.WithoutCancel(ctx))
		}
		return nil, ErrCredentialUnavailable
	}
	state := &generationSecretsState{
		generation: set.DesiredRevision,
		allowed:    buildGenerationClosureIndex(owned),
		encryption: materializer.encryption,
		resolver:   resolver,
		resources:  newGenerationResourceSet(),
	}
	return &generationMaterialization{state: state}, nil
}

type generationMaterialization struct {
	state *generationSecretsState
}

func (materialization *generationMaterialization) Secrets() GenerationSecrets {
	if materialization == nil {
		return GenerationSecrets{}
	}
	return GenerationSecrets{state: materialization.state}
}

func (materialization *generationMaterialization) Close(ctx context.Context) error {
	if materialization == nil || materialization.state == nil {
		return ErrInvalidCapability
	}
	return materialization.state.close(ctx)
}

type generationSecretsState struct {
	generation uint64
	allowed    closureIndex
	encryption data_encryption.Service
	resolver   GenerationResolver
	resources  *generationResourceSet

	gate      sync.RWMutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// GenerationSecrets is the generation-local view injected into plugins. It
// has no lifecycle authority and cannot be recreated from provider tickets.
type GenerationSecrets struct {
	state *generationSecretsState
}

func (secrets GenerationSecrets) Generation() uint64 {
	if secrets.state == nil {
		return 0
	}
	return secrets.state.generation
}

func (secrets GenerationSecrets) Valid() bool {
	if secrets.state == nil || secrets.state.generation == 0 || secrets.state.resources == nil {
		return false
	}
	secrets.state.gate.RLock()
	defer secrets.state.gate.RUnlock()
	return !secrets.state.closed
}

func (secrets GenerationSecrets) SameGeneration(other GenerationSecrets) bool {
	return secrets.Valid() && other.Valid() && secrets.state == other.state
}

func (secrets GenerationSecrets) SharedLimiter(name string, capacity int) (GenerationLimiter, error) {
	if !secrets.Valid() {
		return GenerationLimiter{}, ErrInvalidCapability
	}
	return secrets.state.resources.limiter(name, capacity)
}

func (secrets GenerationSecrets) Materialize(
	ctx context.Context,
	scope Scope,
	raw string,
) (Value, error) {
	if secrets.state == nil {
		return Value{}, ErrInvalidCapability
	}
	ctx = normalizeContext(ctx)
	secrets.state.gate.RLock()
	defer secrets.state.gate.RUnlock()
	if secrets.state.closed {
		return Value{}, ErrCredentialUnavailable
	}
	if err := validateScope(scope); err != nil {
		return Value{}, err
	}
	if scope.Generation != secrets.state.generation ||
		!secrets.state.allowed.Contains(scope.Domain, scope.Resource) {
		return Value{}, ErrCapabilityScopeMismatch
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if _, err := secrets.state.encryption.ValidateDeclaration(scope.Plugin, scope.Source, scope.Field); err != nil {
		return Value{}, ErrInvalidScope
	}
	resolved, err := secrets.state.encryption.ResolveDeclared(scope.Plugin, scope.Source, scope.Field, raw)
	if err != nil {
		return Value{}, ErrCredentialUnavailable
	}
	if isReference(resolved) {
		resolved, err = secrets.state.resolver.ResolveReference(ctx, scope, resolved)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return Value{}, contextErr
			}
			return Value{}, ErrCredentialUnavailable
		}
	}
	return newValue(resolved), nil
}

func (state *generationSecretsState) close(ctx context.Context) error {
	ctx = normalizeContext(ctx)
	state.closeOnce.Do(func() {
		state.gate.Lock()
		state.closed = true
		state.resources.stop()
		resolver := state.resolver
		state.resolver = nil
		state.allowed = nil
		state.gate.Unlock()
		if resolver != nil && resolver.Close(context.WithoutCancel(ctx)) != nil {
			state.closeErr = ErrCredentialUnavailable
		}
	})
	return state.closeErr
}

func validateGenerationPublication(set generation.PublicationSet) error {
	if set.DesiredRevision == 0 || len(set.Domains) == 0 {
		return ErrInvalidCapability
	}
	for domain, candidate := range set.Domains {
		if domain != generation.DomainHTTP && domain != generation.DomainStream {
			return ErrInvalidCapability
		}
		if err := generation.ValidatePublicationCandidate(
			domain, set.DesiredRevision, candidate,
		); err != nil {
			return ErrInvalidCapability
		}
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

func buildGenerationClosureIndex(set generation.PublicationSet) closureIndex {
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

func validateScope(scope Scope) error {
	if scope.Generation == 0 ||
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

func clonePublicationSet(set generation.PublicationSet) generation.PublicationSet {
	owned := generation.PublicationSet{
		DesiredRevision: set.DesiredRevision,
		Domains:         make(map[generation.Domain]generation.PublicationCandidate, len(set.Domains)),
	}
	for domain, candidate := range set.Domains {
		owned.Domains[domain] = generation.PublicationCandidate{
			Artifact: candidate.Artifact, Snapshot: candidate.Snapshot.Clone(),
			Closure:   append([]generation.ResourceKey(nil), candidate.Closure...),
			Decisions: append([]generation.ResourceDecision(nil), candidate.Decisions...),
		}
	}
	return owned
}
