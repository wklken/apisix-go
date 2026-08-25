package compiler

import (
	"context"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestDecodeStreamResourceSetUsesOnlyPublishedStreamClosure(t *testing.T) {
	t.Parallel()
	snapshot := mustGenerationSnapshot(t, 81, []generation.Resource{
		resourceValue("stream_routes", "b", `{"id":"b","upstream_id":"u1"}`),
		resourceValue("stream_routes", "a", `{"id":"a","service_id":"s1"}`),
		resourceValue("services", "s1", `{"id":"s1","upstream_id":"u1"}`),
		resourceValue("upstreams", "u1", `{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}`),
		resourceValue("plugins", "plugins", `[{"name":"request-id"},{"name":"mqtt-proxy","stream":true}]`),
	}, nil)
	candidate := compileDomain(t, generation.DomainStream, snapshot, generation.PublishedGeneration{}, false)

	resources, err := decodeStreamResourceSet(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if resources.revision != 81 || len(resources.routes) != 2 ||
		resources.routes[0].ID != "a" || resources.routes[1].ID != "b" {
		t.Fatalf("routes = %#v at revision %d", resources.routes, resources.revision)
	}
	if resources.services["s1"].UpstreamID != "u1" || len(resources.upstreams["u1"].Nodes) != 1 {
		t.Fatalf("dependency closure = %#v / %#v", resources.services, resources.upstreams)
	}
	if !resources.dynamicPlugins || !slices.Equal(resources.enabledPlugins, []string{"mqtt-proxy"}) {
		t.Fatalf("enabled stream plugins = %v/%v", resources.enabledPlugins, resources.dynamicPlugins)
	}
}

func TestDecodeStreamResourceSetPreservesDynamicPluginsAbsentVersusPresentEmpty(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		plugins *generation.Resource
		present bool
	}{
		{name: "absent"},
		{name: "present-empty", plugins: func() *generation.Resource {
			value := resourceValue("plugins", "plugins", `[]`)
			return &value
		}(), present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resources := []generation.Resource{
				resourceValue("stream_routes", "r1", `{"id":"r1","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}}`),
			}
			if test.plugins != nil {
				resources = append(resources, *test.plugins)
			}
			snapshot := mustGenerationSnapshot(t, 82, resources, nil)
			candidate := compileDomain(t, generation.DomainStream, snapshot, generation.PublishedGeneration{}, false)
			decoded, err := decodeStreamResourceSet(context.Background(), candidate)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.dynamicPlugins != test.present {
				t.Fatalf("dynamicPlugins = %v, want %v", decoded.dynamicPlugins, test.present)
			}
			if test.present && decoded.enabledPlugins == nil {
				t.Fatal("present empty /plugins decoded as nil")
			}
		})
	}
}

func TestDecodeStreamResourceSetRejectsInvalidCandidate(t *testing.T) {
	t.Parallel()
	snapshot := mustGenerationSnapshot(t, 83, []generation.Resource{
		resourceValue("stream_routes", "r1", `{"id":"r1","upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}}`),
	}, nil)
	_, err := decodeStreamResourceSet(context.Background(), generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainStream, Revision: 83, Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
	})
	if err == nil {
		t.Fatal("decodeStreamResourceSet() error = nil")
	}
}
