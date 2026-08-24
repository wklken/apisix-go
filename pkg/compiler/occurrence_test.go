package compiler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

func TestFinalFactoryOccurrencesUseOnlyPublishedDomainScopedResources(t *testing.T) {
	desired := mustGenerationSnapshot(t, 31, []generation.Resource{
		resourceValue("services", "s1", `{"id":"s1","plugins":{"request-id":{}}}`),
		resourceValue("routes", "r1", `{"id":"r1","uri":"/","plugins":{"request-id":{}}}`),
		resourceValue(
			"consumers",
			"c1",
			`{"username":"c1","plugins":{"basic-auth":{"username":"u","password":"$ENV://PASSWORD"}}}`,
		),
		resourceValue(
			"plugin_metadata",
			"http-logger",
			`{}`,
		),
		resourceValue("plugins", "plugins", `[{"name":"request-id"}]`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP, generation.DomainStream)
	compiler := newTestCompiler(t)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := finalFactoryOccurrences(context.Background(), ticket, set, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	want := []factoryOccurrenceSpec{
		{
			domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "consumers", ID: "c1"},
			source: capability.SecretConsumerConfig, factory: "basic-auth",
		},
		{
			domain:   generation.DomainHTTP,
			resource: generation.ResourceKey{Kind: "plugin_metadata", ID: "http-logger"},
			source:   capability.SecretPluginMetadata, factory: "http-logger",
		},
		{
			domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
			source: capability.SecretPluginConfig, factory: "request-id",
		},
		{
			domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "services", ID: "s1"},
			source: capability.SecretPluginConfig, factory: "request-id",
		},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("finalFactoryOccurrences() = %#v, want %#v", got, want)
	}
}

func TestFinalFactoryOccurrencesRejectForgedAndCanceledInputs(t *testing.T) {
	desired := mustGenerationSnapshot(t, 32, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","plugins":{"request-id":{}}}`),
	}, nil)
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	compiler := newTestCompiler(t)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	forged := clonePublicationSetForPreparation(set)
	forged.Domains[generation.DomainHTTP] = generation.PublicationCandidate{}
	if _, err := finalFactoryOccurrences(context.Background(), ticket, forged, compiler.schemas); err == nil {
		t.Fatal("finalFactoryOccurrences(forged) error = nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := finalFactoryOccurrences(ctx, ticket, set, compiler.schemas); err == nil {
		t.Fatal("finalFactoryOccurrences(canceled) error = nil")
	}
}

func TestFinalFactoryOccurrencesIgnorePluginMetadataTombstones(t *testing.T) {
	desired := mustGenerationSnapshot(t, 33, nil, []generation.Tombstone{{
		Key:      generation.ResourceKey{Kind: "plugin_metadata", ID: "http-logger"},
		Revision: 33,
	}})
	ticket := ticketForSnapshot(desired, generation.DomainHTTP)
	compiler := newTestCompiler(t)
	set, err := compiler.PreparePublication(context.Background(), ticket, desired, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := finalFactoryOccurrences(context.Background(), ticket, set, compiler.schemas)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("plugin metadata tombstone occurrences = %#v, want none", got)
	}
}

func TestValidateScopedSecretSupportDoesNotAcceptPhantomNoOps(t *testing.T) {
	compiler := newUnsupportedPluginTargetTestCompiler(t)
	undeclared := factoryOccurrenceSpec{
		domain: generation.DomainHTTP, resource: generation.ResourceKey{Kind: "routes", ID: "r1"},
		source: capability.SecretPluginConfig, factory: "request-id",
	}
	if err := validateScopedSecretSupport([]factoryOccurrenceSpec{undeclared}, compiler.schemas.catalog); err != nil {
		t.Fatalf("undeclared factory support error = %v", err)
	}

	compilerDiscard := undeclared
	compilerDiscard.factory = "basic-auth"
	if err := validateScopedSecretSupport(
		[]factoryOccurrenceSpec{compilerDiscard, compilerDiscard},
		compiler.schemas.catalog,
	); err != nil {
		t.Fatalf("compiler-discard factory support error = %v", err)
	}

	realDeclaration := undeclared
	realDeclaration.factory = "echo"
	err := validateScopedSecretSupport([]factoryOccurrenceSpec{realDeclaration}, compiler.schemas.catalog)
	if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "echo") {
		t.Fatalf("unowned plugin-target factory error = %v, want redacted ErrInvalidInput", err)
	}

	compilerDiscard.source = capability.SecretConsumerConfig
	if err := validateScopedSecretSupport(
		[]factoryOccurrenceSpec{compilerDiscard},
		compiler.schemas.catalog,
	); err != nil {
		t.Fatalf("consumer occurrence incorrectly required plugin scoped support: %v", err)
	}
}

func newUnsupportedPluginTargetTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	manifest := mustManifest(t)
	found := false
	for index := range manifest.Plugins {
		pluginCapability := &manifest.Plugins[index]
		for _, factory := range pluginCapability.Factories {
			if factory.Key != "echo" {
				continue
			}
			pluginCapability.SecretDeclarations = append(
				pluginCapability.SecretDeclarations,
				capability.SecretDeclaration{
					Factory: "echo", Source: capability.SecretPluginConfig, Field: "body", Strict: false,
				},
			)
			found = true
		}
	}
	if !found {
		t.Fatal("echo capability is unavailable for unsupported-owner fixture")
	}
	compiler, err := New(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return compiler
}
