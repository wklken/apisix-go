package compiler

import (
	"context"
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestDecodeHTTPResourceSetUsesOnlyPublishedCandidate(t *testing.T) {
	t.Parallel()

	snapshot, err := generation.NewSnapshot(17, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","uri":"/before","service_id":"s1"}`),
		resourceValue("services", "s1", `{"id":"s1","upstream_id":"u1"}`),
		resourceValue("upstreams", "u1", `{"id":"u1","nodes":{"127.0.0.1:9080":1}}`),
		resourceValue("plugin_configs", "pc1", `{"id":"pc1","plugins":{"request-id":{}}}`),
		resourceValue("protos", "root.proto", `{"id":"root.proto","content":"syntax = proto3;"}`),
		resourceValue("global_rules", "g1", `{"id":"g1","plugins":{"cors":{}}}`),
		resourceValue("ssls", "ssl1", `{"id":"ssl1","sni":"api.example.test","cert":"cert","key":"key"}`),
		resourceValue("consumers", "alice", `{"username":"alice","plugins":{}}`),
		resourceValue("consumer_groups", "staff", `{"id":"staff","plugins":{}}`),
		resourceValue("plugins", "plugins", `[{"name":"request-id"},{"name":"mqtt-proxy","stream":true}]`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := compileDomain(t, generation.DomainHTTP, snapshot, generation.PublishedGeneration{}, false)

	resources, err := decodeHTTPResourceSet(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if resources.revision != 17 || len(resources.routes) != 1 || resources.routes[0].ID != "r1" {
		t.Fatalf("decoded routes = %#v at revision %d", resources.routes, resources.revision)
	}
	if resources.services["s1"].UpstreamID != "u1" || len(resources.upstreams["u1"].Nodes) != 1 {
		t.Fatalf("decoded dependency closure = %#v / %#v", resources.services, resources.upstreams)
	}
	if len(resources.pluginConfigs) != 1 || len(resources.globalRules) != 1 || len(resources.ssls) != 1 {
		t.Fatalf("decoded HTTP resources are incomplete: %#v", resources)
	}
	if len(resources.protos) != 1 || resources.protos["root.proto"] != "syntax = proto3;" {
		t.Fatalf("decoded HTTP protos = %#v", resources.protos)
	}
	if !resources.dynamicPlugins || !slices.Equal(resources.enabledPlugins, []string{"request-id"}) {
		t.Fatalf("enabled plugins = %v/%v", resources.enabledPlugins, resources.dynamicPlugins)
	}
	if !slices.Equal(resources.consumerIDs, []string{"alice"}) ||
		!slices.Equal(resources.consumerGroupIDs, []string{"staff"}) {
		t.Fatalf("consumer identities = %v/%v", resources.consumerIDs, resources.consumerGroupIDs)
	}
}

func TestDecodeHTTPResourceSetRejectsInvalidCandidate(t *testing.T) {
	t.Parallel()

	snapshot, err := generation.NewSnapshot(18, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"different","uri":"/"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeHTTPResourceSet(context.Background(), generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: 18, Digest: snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
	})
	if err == nil {
		t.Fatal("decodeHTTPResourceSet() error = nil")
	}
}
