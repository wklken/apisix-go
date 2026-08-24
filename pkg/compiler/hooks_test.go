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
)

type hookAttemptRegistration struct {
	id    secret.AttemptID
	calls int
	scope secret.Scope
}

func (registration *hookAttemptRegistration) AttemptID() secret.AttemptID { return registration.id }

func (registration *hookAttemptRegistration) Materialize(
	_ context.Context,
	scope secret.Scope,
	_ string,
) (secret.Value, error) {
	registration.calls++
	registration.scope = scope
	return secret.Value{}, nil
}

func (*hookAttemptRegistration) Close(context.Context) error { return nil }

func TestPreparationAttemptBindsAuthorityAndDefensivelyCopiesCandidates(t *testing.T) {
	registration := &hookAttemptRegistration{id: secret.AttemptID{1}}
	capabilityValue, err := secret.NewGenerationCapability(registration, 7)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := mustGenerationSnapshot(t, 7, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1"}`),
	}, nil)
	candidate := publishedForDomain(generation.DomainHTTP, snapshot)
	spec := factoryOccurrenceSpec{
		domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		source: capability.SecretPluginConfig, factory: "request-id",
	}
	attempt, err := newPreparationAttempt(
		7,
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: generation.PublicationCandidate(candidate),
		},
		capabilityValue,
		[]factoryOccurrenceSpec{spec},
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Generation() != 7 || attempt.AttemptID() != registration.id {
		t.Fatalf("attempt identity = %d/%x", attempt.Generation(), attempt.AttemptID())
	}

	got, ok := attempt.Candidate(generation.DomainHTTP)
	if !ok {
		t.Fatal("Candidate(http) missing")
	}
	got.Closure[0].ID = "mutated"
	resources := got.Snapshot.Resources()
	resources[0].Value[0]++
	again, ok := attempt.Candidate(generation.DomainHTTP)
	if !ok || again.Closure[0].ID == "mutated" ||
		slices.Equal(resources[0].Value, again.Snapshot.Resources()[0].Value) {
		t.Fatal("Candidate() exposed attempt-owned state")
	}
	if _, ok := attempt.Candidate(generation.DomainStream); ok {
		t.Fatal("Candidate(stream) unexpectedly present")
	}

	occurrences := attempt.Occurrences(capability.SecretPluginConfig)
	if len(occurrences) != 1 || occurrences[0].Factory() != "request-id" {
		t.Fatalf("Occurrences() = %#v", occurrences)
	}
	occurrences[0] = FactoryOccurrence{}
	if again := attempt.Occurrences(capability.SecretPluginConfig); len(again) != 1 || again[0].Factory() == "" {
		t.Fatal("Occurrences() exposed attempt-owned slice")
	}
}

func TestPreparationAttemptRejectsForeignOccurrenceBeforeSecretAccess(t *testing.T) {
	firstRegistration := &hookAttemptRegistration{id: secret.AttemptID{1}}
	secondRegistration := &hookAttemptRegistration{id: secret.AttemptID{2}}
	first := mustPreparationAttemptForTest(t, firstRegistration)
	second := mustPreparationAttemptForTest(t, secondRegistration)
	occurrence := first.Occurrences(capability.SecretPluginConfig)[0]

	if _, err := second.MaterializeSecret(
		context.Background(), occurrence, "value", "$ENV://VALUE",
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("foreign occurrence error = %v, want ErrInvalidInput", err)
	}
	if secondRegistration.calls != 0 {
		t.Fatal("foreign occurrence reached secret registration")
	}
	if _, err := first.MaterializeSecret(
		context.Background(), occurrence, "value", "$ENV://VALUE",
	); err != nil {
		t.Fatal(err)
	}
	if firstRegistration.calls != 1 || firstRegistration.scope.Plugin != "request-id" ||
		firstRegistration.scope.Resource.ID != "r1" || firstRegistration.scope.Field != "value" {
		t.Fatalf("materialized scope = %#v", firstRegistration.scope)
	}
}

func TestPreparationAttemptRequiresExactFactoryBinding(t *testing.T) {
	attempt := mustPreparationAttemptForTest(t, &hookAttemptRegistration{id: secret.AttemptID{1}})
	occurrence := attempt.Occurrences(capability.SecretPluginConfig)[0]
	wrong, err := plugin.NewFactoryInstance("echo", base.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.PrepareScopedPluginSecrets(
		context.Background(), occurrence, wrong,
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong factory binding error = %v, want ErrInvalidInput", err)
	}
	if err := attempt.PrepareScopedPluginSecrets(
		context.Background(), occurrence, plugin.FactoryInstance{},
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero factory binding error = %v, want ErrInvalidInput", err)
	}
}

func mustPreparationAttemptForTest(
	t *testing.T,
	registration *hookAttemptRegistration,
) PreparationAttempt {
	t.Helper()
	capabilityValue, err := secret.NewGenerationCapability(registration, 7)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := newPreparationAttempt(7, nil, capabilityValue, []factoryOccurrenceSpec{{
		domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		source: capability.SecretPluginConfig, factory: "request-id",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}
