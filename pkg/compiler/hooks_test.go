package compiler

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type hookSecretResolver struct {
	calls int
	scope secret.Scope
}

func (resolver *hookSecretResolver) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	resolver.calls++
	resolver.scope = scope
	return raw, nil
}

func TestPreparationGenerationDefensivelyCopiesCandidates(t *testing.T) {
	preparation := mustPreparationGenerationForTest(t, 7, &hookSecretResolver{})
	if preparation.Generation() != 7 {
		t.Fatalf("generation = %d, want 7", preparation.Generation())
	}
	got, ok := preparation.Candidate(generation.DomainHTTP)
	if !ok {
		t.Fatal("Candidate(http) missing")
	}
	got.Closure[0].ID = "mutated"
	resources := got.Snapshot.Resources()
	resources[0].Value[0]++
	again, ok := preparation.Candidate(generation.DomainHTTP)
	if !ok || again.Closure[0].ID == "mutated" ||
		slices.Equal(resources[0].Value, again.Snapshot.Resources()[0].Value) {
		t.Fatal("Candidate() exposed preparation-owned state")
	}
	occurrences := preparation.Occurrences(capability.SecretPluginConfig)
	if len(occurrences) != 1 || occurrences[0].Factory() != "http-logger" {
		t.Fatalf("Occurrences() = %#v", occurrences)
	}
	occurrences[0] = FactoryOccurrence{}
	if again := preparation.Occurrences(capability.SecretPluginConfig); len(again) != 1 || again[0].Factory() == "" {
		t.Fatal("Occurrences() exposed preparation-owned slice")
	}
}

func TestPreparationGenerationRejectsForeignOccurrenceBeforeSecretAccess(t *testing.T) {
	resolver := &hookSecretResolver{}
	preparation := mustPreparationGenerationForTest(t, 7, resolver)
	occurrence := preparation.Occurrences(capability.SecretPluginConfig)[0]
	foreign := occurrence
	foreign.resource.ID = "foreign"
	if _, err := preparation.MaterializeSecret(
		context.Background(), foreign, "auth_header", "$ENV://VALUE",
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("foreign occurrence error = %v, want ErrInvalidInput", err)
	}
	if resolver.calls != 0 {
		t.Fatal("foreign occurrence reached secret resolver")
	}
	if _, err := preparation.MaterializeSecret(
		context.Background(), occurrence, "auth_header", "$ENV://VALUE",
	); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 || resolver.scope.Plugin != "http-logger" ||
		resolver.scope.Resource.ID != "r1" || resolver.scope.Field != "auth_header" {
		t.Fatalf("materialized scope = %#v", resolver.scope)
	}
}

func TestPreparationGenerationRejectsOccurrenceFromAnotherMaterialization(t *testing.T) {
	for _, tt := range []struct {
		name           string
		firstRevision  uint64
		secondRevision uint64
	}{
		{name: "different revision", firstRevision: 7, secondRevision: 8},
		{name: "same revision", firstRevision: 7, secondRevision: 7},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := mustPreparationGenerationForTest(t, tt.firstRevision, &hookSecretResolver{})
			secondResolver := &hookSecretResolver{}
			second := mustPreparationGenerationForTest(t, tt.secondRevision, secondResolver)
			foreign := first.Occurrences(capability.SecretPluginConfig)[0]

			if _, err := second.MaterializeSecret(
				context.Background(), foreign, "auth_header", "$ENV://VALUE",
			); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("foreign occurrence error = %v, want ErrInvalidInput", err)
			}
			if secondResolver.calls != 0 {
				t.Fatal("foreign occurrence reached secret resolver")
			}
		})
	}
}

func TestPreparationGenerationRequiresExactFactoryBinding(t *testing.T) {
	preparation := mustPreparationGenerationForTest(t, 7, &hookSecretResolver{})
	occurrence := preparation.Occurrences(capability.SecretPluginConfig)[0]
	wrong, err := plugin.NewFactoryInstance("echo", base.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := preparation.PrepareScopedPluginSecrets(
		context.Background(), occurrence, wrong,
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong factory binding error = %v, want ErrInvalidInput", err)
	}
	if err := preparation.PrepareScopedPluginSecrets(
		context.Background(), occurrence, plugin.FactoryInstance{},
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero factory binding error = %v, want ErrInvalidInput", err)
	}
}

func mustPreparationGenerationForTest(
	t *testing.T,
	revision uint64,
	resolver testutil.SecretResolver,
) PreparationGeneration {
	t.Helper()
	compiler := newTestCompiler(t)
	resource := generation.ResourceKey{Kind: "routes", ID: "r1"}
	snapshot := mustGenerationSnapshot(t, revision, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1"}`),
	}, nil)
	published := publishedForDomain(generation.DomainHTTP, snapshot)
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: generation.PublicationCandidate(published),
		},
	}
	materialization, err := testutil.NewSecretMaterializer(
		resolver, compiler.schemas.catalog,
	).PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = materialization.Close(context.Background()) })
	preparation, err := newPreparationGeneration(
		revision,
		set.Domains,
		materialization.Secrets(),
		[]factoryOccurrenceSpec{{
			domain: generation.DomainHTTP, resource: resource,
			source: capability.SecretPluginConfig, factory: "http-logger",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return preparation
}
