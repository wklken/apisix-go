package testutil

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/secret"
)

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

func (*scopedSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by scoped secret test fixtures")
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
	registration, err := secret.NewScopedMaterializer(
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
