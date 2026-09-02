package compiler

import (
	"context"
	"slices"
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

func TestFactoryOccurrencesIgnoreDisabledPluginConfigs(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 34, []generation.Resource{
		resourceValue(
			"routes",
			"disabled-route",
			`{"id":"disabled-route","plugins":{"key-auth":{"_meta":{"disable":true},"key":"$secret://vault/team/key"}}}`,
		),
		resourceValue(
			"consumers",
			"disabled-consumer",
			`{"username":"disabled-consumer","plugins":{"basic-auth":{"_meta":{"disable":true},"username":"user","password":"$secret://vault/team/password"}}}`,
		),
	}, nil)
	compiler := newTestCompiler(t)

	got, err := factoryOccurrencesFromCandidates(
		context.Background(),
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {Snapshot: snapshot},
		},
		compiler.schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("disabled plugin occurrences = %#v, want none", got)
	}
}
