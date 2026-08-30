package testutil

import (
	"context"
	"fmt"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
)

// SecretResolver is the narrow test seam for external reference resolution.
type SecretResolver interface {
	ResolveScoped(context.Context, secret.Scope, string) (string, error)
}

type secretResolverFactory struct {
	resolver SecretResolver
}

func (factory *secretResolverFactory) OpenGeneration(
	_ context.Context,
	_ uint64,
	_ generation.PublicationSet,
) (secret.GenerationResolver, error) {
	if factory == nil || factory.resolver == nil {
		return nil, secret.ErrInvalidCapability
	}
	return &generationResolver{resolver: factory.resolver}, nil
}

type generationResolver struct {
	resolver SecretResolver
}

func (resolver *generationResolver) ResolveReference(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	return resolver.resolver.ResolveScoped(ctx, scope, raw)
}

func (*generationResolver) Close(context.Context) error { return nil }

// NewSecretMaterializer adapts a test broker to the production resolver
// factory. Scope validation, declaration admission, and generation cleanup
// remain owned by secret.NewMaterializer.
func NewSecretMaterializer(
	resolver SecretResolver,
	catalog *capability.SecretDeclarationCatalog,
) secret.Materializer {
	return NewSecretMaterializerWithKeyring(resolver, catalog, nil)
}

// NewSecretMaterializerWithKeyring creates the same configured service used by
// production while keeping test key material at the fixture boundary.
func NewSecretMaterializerWithKeyring(
	resolver SecretResolver,
	catalog *capability.SecretDeclarationCatalog,
	keyring []string,
) secret.Materializer {
	service := data_encryption.NewService(len(keyring) > 0, keyring, catalog)
	return secret.NewMaterializer(service, &secretResolverFactory{resolver: resolver})
}

type scopedSecretBroker struct {
	values map[string]string
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context,
	_ secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

// ScopedSecretHarness creates one HTTP route generation for package and benchmark
// tests that only need ordinary literal-or-mapped secret admission.
func ScopedSecretHarness(
	t testing.TB,
	factory string,
	values map[string]string,
	ticket generation.ApplyTicket,
) (secret.GenerationSecrets, secret.Scope, func()) {
	t.Helper()
	revision := ticket.DesiredRevision
	resourceKey := generation.ResourceKey{Kind: "routes", ID: fmt.Sprintf("scoped-secret-%d", revision)}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: resourceKey, Value: []byte(`{"plugins":{}}`),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{resourceKey},
		Decisions: []generation.ResourceDecision{{
			Key: resourceKey, Disposition: generation.DispositionPublished, Code: "scoped-secret-test",
		}},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	materialization, err := NewSecretMaterializer(
		&scopedSecretBroker{values: values}, catalog,
	).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     factory,
		Resource:   resourceKey,
		Source:     capability.SecretPluginConfig,
	}
	return secrets, scope, func() {
		if closeErr := materialization.Close(context.Background()); closeErr != nil {
			t.Fatalf("close scoped secret fixture: %v", closeErr)
		}
	}
}
