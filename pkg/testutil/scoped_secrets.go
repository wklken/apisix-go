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

// SecretAttemptBroker is the narrow test seam for observing attempt
// authorization, resolution, and cleanup while exercising the production
// secret.Materializer implementation.
type SecretAttemptBroker interface {
	secret.ScopedResolver
	AuthorizeCandidate(
		context.Context,
		secret.AttemptID,
		generation.ApplyTicket,
		generation.PublicationSet,
	) error
	RevokeAttempt(context.Context, secret.AttemptID) error
}

type secretResolverFactory struct {
	broker SecretAttemptBroker
}

func (factory *secretResolverFactory) OpenCandidate(
	ctx context.Context,
	id secret.AttemptID,
	ticket generation.ApplyTicket,
	set generation.PublicationSet,
) (secret.AttemptResolver, error) {
	if factory == nil || factory.broker == nil {
		return nil, secret.ErrInvalidCapability
	}
	if err := factory.broker.AuthorizeCandidate(ctx, id, ticket, set); err != nil {
		return nil, err
	}
	return &secretAttemptResolver{broker: factory.broker, id: id}, nil
}

type secretAttemptResolver struct {
	broker SecretAttemptBroker
	id     secret.AttemptID
}

func (resolver *secretAttemptResolver) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	return resolver.broker.ResolveScoped(ctx, scope, raw)
}

func (resolver *secretAttemptResolver) Close(ctx context.Context) error {
	return resolver.broker.RevokeAttempt(ctx, resolver.id)
}

// NewSecretMaterializer adapts a test broker to the production resolver
// factory. Registration, scope validation, declaration admission, attempt
// ownership, and cleanup remain owned by secret.NewMaterializer.
func NewSecretMaterializer(
	broker SecretAttemptBroker,
	catalog *capability.SecretDeclarationCatalog,
) secret.Materializer {
	return NewSecretMaterializerWithKeyring(broker, catalog, nil)
}

// NewSecretMaterializerWithKeyring creates the same configured service used by
// production while keeping test key material at the fixture boundary.
func NewSecretMaterializerWithKeyring(
	broker SecretAttemptBroker,
	catalog *capability.SecretDeclarationCatalog,
	keyring []string,
) secret.Materializer {
	service := data_encryption.NewService(len(keyring) > 0, keyring, catalog)
	return secret.NewMaterializer(service, &secretResolverFactory{broker: broker})
}

type scopedSecretBroker struct {
	values map[string]string
}

func (*scopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
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

func (*scopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error { return nil }

// ScopedSecretHarness creates one HTTP route attempt for package and benchmark
// tests that only need ordinary literal-or-mapped secret admission.
func ScopedSecretHarness(
	t testing.TB,
	factory string,
	values map[string]string,
	ticket generation.ApplyTicket,
) (secret.GenerationCapability, secret.Scope, func()) {
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
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := NewSecretMaterializer(
		&scopedSecretBroker{values: values}, catalog,
	).RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     factory,
		Resource:   resourceKey,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, func() {
		if closeErr := registration.Close(context.Background()); closeErr != nil {
			t.Fatalf("close scoped secret fixture: %v", closeErr)
		}
	}
}
